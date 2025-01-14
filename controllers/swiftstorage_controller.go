/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	helper "github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	service "github.com/openstack-k8s-operators/lib-common/modules/common/service"
	statefulset "github.com/openstack-k8s-operators/lib-common/modules/common/statefulset"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	swiftv1beta1 "github.com/openstack-k8s-operators/swift-operator/api/v1beta1"
	swift "github.com/openstack-k8s-operators/swift-operator/pkg/swift"

	"github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	"github.com/openstack-k8s-operators/lib-common/modules/common/configmap"
	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	"github.com/openstack-k8s-operators/lib-common/modules/common/util"
)

// SwiftStorageReconciler reconciles a SwiftStorage object
type SwiftStorageReconciler struct {
	client.Client
	Scheme  *runtime.Scheme
	Log     logr.Logger
	Kclient kubernetes.Interface
}

//+kubebuilder:rbac:groups=swift.openstack.org,resources=swiftstorages,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=swift.openstack.org,resources=swiftstorages/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=swift.openstack.org,resources=swiftstorages/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the SwiftStorage object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.12.2/pkg/reconcile
func (r *SwiftStorageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = r.Log.WithValues("swiftstorage", req.NamespacedName)

	instance := &swiftv1beta1.SwiftStorage{}
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			r.Log.Info("SwiftStorage resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		r.Log.Error(err, "Failed to get SwiftStorage")
		return ctrl.Result{}, err
	}

	if instance.Status.Conditions == nil {
		instance.Status.Conditions = condition.Conditions{}
		cl := condition.CreateList(
			condition.UnknownCondition(condition.ReadyCondition, condition.InitReason, condition.ReadyInitMessage),
			condition.UnknownCondition(swiftv1beta1.SwiftStorageReadyCondition, condition.InitReason, condition.ReadyInitMessage),
		)

		instance.Status.Conditions.Init(&cl)

		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	helper, err := helper.NewHelper(
		instance,
		r.Client,
		r.Kclient,
		r.Scheme,
		r.Log,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	ls := swift.GetLabelsStorage()

	// Create a ConfigMap populated with content from templates/
	envVars := make(map[string]env.Setter)
	tpl := getStorageConfigMapTemplates(instance, ls)
	err = configmap.EnsureConfigMaps(ctx, helper, instance, tpl, &envVars)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check if there is a ConfigMap for the Swift rings
	_, ctrlResult, err := configmap.GetConfigMap(ctx, helper, instance, swiftv1beta1.RingConfigMapName, 5*time.Second)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	// Headless Service
	svc := service.NewService(getStorageService(instance), ls, 5*time.Second)
	ctrlResult, err = svc.CreateOrPatch(ctx, helper)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	// Limit internal storage traffic to Swift services
	np := swift.NewNetworkPolicy(getStorageNetworkPolicy(instance), ls, 5*time.Second)
	ctrlResult, err = np.CreateOrPatch(ctx, helper)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	// Ensure the StatefulSet is not resized after initial deployment
	found, err := statefulset.GetStatefulSetWithName(ctx, helper, instance.Name, instance.Namespace)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrlResult, err
	} else if err == nil {
		if *found.Spec.Replicas > instance.Spec.Replicas {
			r.Log.Info(fmt.Sprintf(
				"Downsizing (%d -> %d) number of replicas not supported",
				*found.Spec.Replicas, instance.Spec.Replicas))
			instance.Spec.Replicas = *found.Spec.Replicas
			if err := r.Client.Update(ctx, instance); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Statefulset with all backend containers
	sset := statefulset.NewStatefulSet(getStorageStatefulSet(instance, ls), 5*time.Second)
	ctrlResult, err = sset.CreateOrPatch(ctx, helper)
	if err != nil {
		return ctrlResult, err
	} else if (ctrlResult != ctrl.Result{}) {
		return ctrlResult, nil
	}

	if sset.GetStatefulSet().Status.ReadyReplicas == instance.Spec.Replicas {
		envVars := make(map[string]env.Setter)
		devices, err := getDeviceList(ctx, helper, instance)
		if err != nil {
			return ctrl.Result{}, err
		}
		tpl = getDeviceConfigMapTemplates(instance, devices)
		err = configmap.EnsureConfigMaps(ctx, helper, instance, tpl, &envVars)
		if err != nil {
			return ctrl.Result{}, err
		}
		instance.Status.Conditions.MarkTrue(condition.ReadyCondition, condition.ReadyMessage)
		instance.Status.Conditions.MarkTrue(swiftv1beta1.SwiftStorageReadyCondition, condition.ReadyMessage)
		if err := r.Status().Update(ctx, instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	r.Log.Info(fmt.Sprintf("Reconciled SwiftStorage '%s' successfully", instance.Name))
	return ctrl.Result{}, nil
}

func getStorageConfigMapTemplates(instance *swiftv1beta1.SwiftStorage, labels map[string]string) []util.Template {
	templateParameters := make(map[string]interface{})

	return []util.Template{
		{
			Name:          fmt.Sprintf("%s-config-data", instance.Name),
			Namespace:     instance.Namespace,
			Type:          util.TemplateTypeConfig,
			InstanceType:  instance.Kind,
			Labels:        labels,
			ConfigOptions: templateParameters,
		},
		{
			Name:               fmt.Sprintf("%s-scripts", instance.Name),
			Namespace:          instance.Namespace,
			Type:               util.TemplateTypeScripts,
			InstanceType:       instance.Kind,
			Labels:             labels,
			AdditionalTemplate: map[string]string{"swift-init.sh": "/common/swift-init.sh", "ring-sync.sh": "/common/ring-sync.sh"},
		},
	}
}

func getStorageVolumes(instance *swiftv1beta1.SwiftStorage) []corev1.Volume {
	var scriptsVolumeDefaultMode int32 = 0755
	return []corev1.Volume{
		{
			Name: swift.ClaimName,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: swift.ClaimName,
				},
			},
		},
		{
			Name: "config-data",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: instance.Name + "-config-data",
					},
				},
			},
		},
		{
			Name: "swiftconf",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: instance.Spec.SwiftConfSecret,
				},
			},
		},
		{
			Name: "ring-data",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: swiftv1beta1.RingConfigMapName,
					},
				},
			},
		},
		{
			Name: "config-data-merged",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""},
			},
		},
		{
			Name: "cache",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{Medium: ""},
			},
		},
		{
			Name: "scripts",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					DefaultMode: &scriptsVolumeDefaultMode,
					LocalObjectReference: corev1.LocalObjectReference{
						Name: instance.Name + "-scripts",
					},
				},
			},
		},
	}
}

func getStorageVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{
			Name:      swift.ClaimName,
			MountPath: "/srv/node/d1",
			ReadOnly:  false,
		},
		{
			Name:      "config-data",
			MountPath: "/var/lib/config-data/default",
			ReadOnly:  true,
		},
		{
			Name:      "swiftconf",
			MountPath: "/var/lib/config-data/swiftconf",
			ReadOnly:  true,
		},
		{
			Name:      "ring-data",
			MountPath: "/var/lib/config-data/rings",
			ReadOnly:  true,
		},
		{
			Name:      "config-data-merged",
			MountPath: "/etc/swift",
			ReadOnly:  false,
		},
		{
			Name:      "cache",
			MountPath: "/var/cache/swift",
			ReadOnly:  false,
		},
		{
			Name:      "scripts",
			MountPath: "/usr/local/bin/container-scripts",
			ReadOnly:  true,
		},
	}
}

func getPorts(port int32, name string) []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{
			ContainerPort: port,
			Name:          name,
		},
	}
}

func getStorageInitContainers(swiftstorage *swiftv1beta1.SwiftStorage) []corev1.Container {
	securityContext := swift.GetSecurityContext()

	return []corev1.Container{
		{
			Name:            "swift-init",
			Image:           swiftstorage.Spec.ContainerImageAccount,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/local/bin/container-scripts/swift-init.sh"},
		},
	}
}

func getStorageContainers(swiftstorage *swiftv1beta1.SwiftStorage) []corev1.Container {
	securityContext := swift.GetSecurityContext()

	return []corev1.Container{
		{
			Name:            "account-server",
			Image:           swiftstorage.Spec.ContainerImageAccount,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			Ports:           getPorts(swift.AccountServerPort, "account"),
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-account-server", "/etc/swift/account-server.conf", "-v"},
		},
		{
			Name:            "account-replicator",
			Image:           swiftstorage.Spec.ContainerImageAccount,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-account-replicator", "/etc/swift/account-server.conf", "-v"},
		},
		{
			Name:            "account-auditor",
			Image:           swiftstorage.Spec.ContainerImageAccount,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-account-auditor", "/etc/swift/account-server.conf", "-v"},
		},
		{
			Name:            "account-reaper",
			Image:           swiftstorage.Spec.ContainerImageAccount,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-account-reaper", "/etc/swift/account-server.conf", "-v"},
		},
		{
			Name:            "container-server",
			Image:           swiftstorage.Spec.ContainerImageContainer,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			Ports:           getPorts(swift.ContainerServerPort, "container"),
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-container-server", "/etc/swift/container-server.conf", "-v"},
		},
		{
			Name:            "container-replicator",
			Image:           swiftstorage.Spec.ContainerImageContainer,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-container-replicator", "/etc/swift/container-server.conf", "-v"},
		},
		{
			Name:            "container-auditor",
			Image:           swiftstorage.Spec.ContainerImageContainer,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-container-replicator", "/etc/swift/container-server.conf", "-v"},
		},
		{
			Name:            "container-updater",
			Image:           swiftstorage.Spec.ContainerImageContainer,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-container-replicator", "/etc/swift/container-server.conf", "-v"},
		},
		{
			Name:            "object-server",
			Image:           swiftstorage.Spec.ContainerImageObject,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			Ports:           getPorts(swift.ObjectServerPort, "object"),
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-object-server", "/etc/swift/object-server.conf", "-v"},
		},
		{
			Name:            "object-replicator",
			Image:           swiftstorage.Spec.ContainerImageObject,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-object-replicator", "/etc/swift/object-server.conf", "-v"},
		},
		{
			Name:            "object-auditor",
			Image:           swiftstorage.Spec.ContainerImageObject,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-object-replicator", "/etc/swift/object-server.conf", "-v"},
		},
		{
			Name:            "object-updater",
			Image:           swiftstorage.Spec.ContainerImageObject,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-object-replicator", "/etc/swift/object-server.conf", "-v"},
		},
		{
			Name:            "object-expirer",
			Image:           swiftstorage.Spec.ContainerImageProxy,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/swift-object-expirer", "/etc/swift/object-expirer.conf", "-v"},
		},
		{
			Name:            "rsync",
			Image:           swiftstorage.Spec.ContainerImageObject,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			Ports:           getPorts(swift.RsyncPort, "rsync"),
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/bin/rsync", "--daemon", "--no-detach", "--config=/etc/swift/rsyncd.conf", "--log-file=/dev/stdout"},
		},
		{
			Name:            "memcached",
			Image:           swiftstorage.Spec.ContainerImageMemcached,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			Ports:           getPorts(swift.MemcachedPort, "memcached"),
			Command:         []string{"/usr/bin/memcached", "-p", "11211", "-u", "memcached"},
		},
		{
			Name:            "ring-sync",
			Image:           swiftstorage.Spec.ContainerImageProxy,
			ImagePullPolicy: corev1.PullIfNotPresent,
			SecurityContext: &securityContext,
			VolumeMounts:    getStorageVolumeMounts(),
			Command:         []string{"/usr/local/bin/container-scripts/ring-sync.sh"},
		},
	}
}

func getStorageService(
	swiftstorage *swiftv1beta1.SwiftStorage) *corev1.Service {

	selector := swift.GetLabelsStorage()

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      swiftstorage.Name,
			Namespace: swiftstorage.Namespace,
			Labels:    selector,
		},
		Spec: corev1.ServiceSpec{
			Selector: selector,
			Ports: []corev1.ServicePort{
				{
					Name:     "account",
					Port:     swift.AccountServerPort,
					Protocol: corev1.ProtocolTCP,
				},
				{
					Name:     "container",
					Port:     swift.ContainerServerPort,
					Protocol: corev1.ProtocolTCP,
				},
				{
					Name:     "object",
					Port:     swift.ObjectServerPort,
					Protocol: corev1.ProtocolTCP,
				},
				{
					Name:     "rsync",
					Port:     swift.RsyncPort,
					Protocol: corev1.ProtocolTCP,
				},
			},
			ClusterIP: "None", // headless service
		},
	}
}

func getStorageStatefulSet(
	swiftstorage *swiftv1beta1.SwiftStorage, labels map[string]string) *appsv1.StatefulSet {

	trueVal := true
	OnRootMismatch := corev1.FSGroupChangeOnRootMismatch
	user := int64(swift.RunAsUser)

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      swiftstorage.Name,
			Namespace: swiftstorage.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: swiftstorage.Name,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Replicas: &swiftstorage.Spec.Replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: swift.ServiceAccount,
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup:             &user,
						FSGroupChangePolicy: &OnRootMismatch,
						Sysctls: []corev1.Sysctl{{
							Name:  "net.ipv4.ip_unprivileged_port_start",
							Value: "873",
						}},
						RunAsNonRoot: &trueVal,
						SeccompProfile: &corev1.SeccompProfile{
							Type: corev1.SeccompProfileTypeRuntimeDefault,
						},
					},
					Volumes:        getStorageVolumes(swiftstorage),
					InitContainers: getStorageInitContainers(swiftstorage),
					Containers:     getStorageContainers(swiftstorage),
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{
					Name: swift.ClaimName,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: &swiftstorage.Spec.StorageClass,
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteOnce,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse(swiftstorage.Spec.StorageRequest),
						},
					},
				},
			}},
		},
	}
}

//+kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete

func getStorageNetworkPolicy(
	swiftstorage *swiftv1beta1.SwiftStorage) *networkingv1.NetworkPolicy {

	portAccountServer := intstr.FromInt(int(swift.AccountServerPort))
	portContainerServer := intstr.FromInt(int(swift.ContainerServerPort))
	portObjectServer := intstr.FromInt(int(swift.ObjectServerPort))
	portRsync := intstr.FromInt(int(swift.RsyncPort))

	storageLabels := swift.GetLabelsStorage()
	proxyLabels := swift.GetLabelsProxy()

	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "np-" + swiftstorage.Name,
			Namespace: swiftstorage.Namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: storageLabels,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Port: &portAccountServer,
						},
						{
							Port: &portContainerServer,
						},
						{
							Port: &portObjectServer,
						},
						{
							Port: &portRsync,
						},
					},
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: storageLabels,
							},
						},
					},
				},
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Port: &portAccountServer,
						},
						{
							Port: &portContainerServer,
						},
						{
							Port: &portObjectServer,
						},
					},
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: proxyLabels,
							},
						},
					},
				},
			},
		},
	}
}

//+kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch

func getDeviceList(ctx context.Context, h *helper.Helper, instance *swiftv1beta1.SwiftStorage) (string, error) {
	var devices strings.Builder

	foundClaim := &corev1.PersistentVolumeClaim{}
	for replica := 0; replica < int(instance.Spec.Replicas); replica++ {
		cn := fmt.Sprintf("%s-%s-%d", swift.ClaimName, instance.Name, replica)
		err := h.GetClient().Get(ctx, types.NamespacedName{Name: cn, Namespace: instance.Namespace}, foundClaim)
		if err == nil {
			fsc := foundClaim.Status.Capacity["storage"]
			c, _ := (&fsc).AsInt64()
			c = c / (1000 * 1000 * 1000)
			host := fmt.Sprintf("%s-%d.%s", instance.Name, replica, instance.Name)
			devices.WriteString(fmt.Sprintf("%s,%s,%d\n", host, "d1", c))
		} else {
			return "", err
		}
	}
	return devices.String(), nil
}

func getDeviceConfigMapTemplates(instance *swiftv1beta1.SwiftStorage, devices string) []util.Template {
	data := make(map[string]string)
	data["devices.csv"] = devices

	return []util.Template{
		{
			Name:         swiftv1beta1.DeviceConfigMapName,
			Namespace:    instance.Namespace,
			Type:         util.TemplateTypeNone,
			InstanceType: instance.Kind,
			CustomData:   data,
		},
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *SwiftStorageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&swiftv1beta1.SwiftStorage{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}
