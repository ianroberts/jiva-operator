/*
Copyright 2021.

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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docker/go-units"

	"github.com/openebs/jiva-operator/pkg/jiva"
	"github.com/openebs/jiva-operator/pkg/kubernetes/container"
	deploy "github.com/openebs/jiva-operator/pkg/kubernetes/deployment"
	pts "github.com/openebs/jiva-operator/pkg/kubernetes/podtemplatespec"
	"github.com/openebs/jiva-operator/pkg/kubernetes/pvc"
	svc "github.com/openebs/jiva-operator/pkg/kubernetes/service"
	sts "github.com/openebs/jiva-operator/pkg/kubernetes/statefulset"
	"github.com/openebs/jiva-operator/pkg/volume"
	"github.com/openebs/jiva-operator/version"
	operr "github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1beta1 "k8s.io/api/policy/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	openebsiov1alpha1 "github.com/openebs/jiva-operator/pkg/apis/openebs/v1alpha1"
)

// JivaVolumeReconciler reconciles a JivaVolume object
type JivaVolumeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

type upgradeParams struct {
	j      *openebsiov1alpha1.JivaVolume
	client client.Client
}

type upgradeFunc func(u *upgradeParams) (*openebsiov1alpha1.JivaVolume, error)

var (
	upgradeMap  = map[string]upgradeFunc{}
	podIPMap    = map[string]string{}
	selectorMap = map[string]string{}
)

const (
	pdbAPIVersion            = "policyv1beta1"
	defaultStorageClass      = "openebs-hostpath"
	replicaAntiAffinityKey   = "openebs.io/replica-anti-affinity"
	defaultReplicationFactor = 3
	defaultDisableMonitor    = false
	openebsPVC               = "openebs.io/persistent-volume-claim"
)

type policyOptFuncs func(*openebsiov1alpha1.JivaVolumePolicySpec, openebsiov1alpha1.JivaVolumePolicySpec)

var (
	installFuncs = []func(r *JivaVolumeReconciler, cr *openebsiov1alpha1.JivaVolume) error{
		populateJivaVolumePolicy,
		createControllerService,
		createControllerDeployment,
		createReplicaStatefulSet,
		createReplicaPodDisruptionBudget,
	}

	updateErrMsg = "failed to update JivaVolume with service info"

	defaultServiceAccountName = os.Getenv("OPENEBS_SERVICEACCOUNT_NAME")
)

// +kubebuilder:rbac:groups=openebs.io.openebs.io,resources=jivavolumes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=openebs.io.openebs.io,resources=jivavolumes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=openebs.io.openebs.io,resources=jivavolumes/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the JivaVolume object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *JivaVolumeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// Fetch the JivaVolume instance
	instance := &openebsiov1alpha1.JivaVolume{}
	err := r.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	err = r.reconcileVersion(instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	ok, err := r.shouldReconcile(instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	if _, ok := podIPMap[instance.Name]; !ok {
		err = r.updatePodIPMap(instance)
		if err != nil {
			// log err only, as controller must be in container creating state
			// don't return err as it will dump stack trace unneccesary
			logrus.Infof("not able to get controller pod ip for volume %s: %s", instance.Name, err.Error())
			time.Sleep(1 * time.Second)
		}
	}

	// initially Phase will be "", so it will skip switch case
	// Once it has started boostrapping it will set the Phase to Pending/Failed
	// depends upon the error. If bootstrap is successful it will set the Phase
	// to syncing which will be changed to Ready later when volume becomes RW
	switch instance.Status.Phase {
	case openebsiov1alpha1.JivaVolumePhaseReady:
		// fetching the latest status before performing
		// other operations
		err = r.getAndUpdateVolumeStatus(instance)
		if err != nil {
			return reconcile.Result{}, err
		}
		if r.isScaleup(instance) {
			logrus.Info("performing scaleup operation on " + instance.Name)
			err = r.performScaleup(instance)
			if err != nil {
				r.Recorder.Eventf(instance, corev1.EventTypeWarning,
					"ReplicaScaleup", "failed to scaleup volume, due to error: %v", err)
				return reconcile.Result{}, fmt.Errorf("failed to scaleup volume %s: %s",
					instance.Name, err.Error())
			}
			return reconcile.Result{}, r.getAndUpdateVolumeStatus(instance)
		}
		if err := r.moveReplicasForMissingNodes(instance); err != nil {
			r.Recorder.Eventf(instance, corev1.EventTypeWarning,
				"ReplicaMovement", "failed to move replica, due to error: %v", err)
			return reconcile.Result{}, fmt.Errorf("failed to move replica %s: %s",
				instance.Name, err.Error())
		}
		return reconcile.Result{}, nil
	case openebsiov1alpha1.JivaVolumePhaseSyncing, openebsiov1alpha1.JivaVolumePhaseUnkown:
		return reconcile.Result{}, r.getAndUpdateVolumeStatus(instance)
	case openebsiov1alpha1.JivaVolumePhaseDeleting:
		logrus.Info("start tearing down jiva components", "JivaVolume: ", instance.Name)
		return reconcile.Result{}, nil
	case "", openebsiov1alpha1.JivaVolumePhasePending, openebsiov1alpha1.JivaVolumePhaseFailed:
		if ok {
			logrus.Info("start bootstraping jiva components", "JivaVolume: ", instance.Name)
			return reconcile.Result{}, r.bootstrapJiva(instance)
		}
	}

	return reconcile.Result{}, nil
}

func (r *JivaVolumeReconciler) updatePodIPMap(cr *openebsiov1alpha1.JivaVolume) error {
	var (
		controllerLabel = "openebs.io/component=jiva-controller,openebs.io/persistent-volume="
	)

	labelSelector, _ := labels.Parse(
		controllerLabel + cr.Name)

	pods := corev1.PodList{}
	err := r.List(context.TODO(), &pods, &client.ListOptions{
		Namespace:     cr.Namespace,
		LabelSelector: labelSelector,
		FieldSelector: fields.SelectorFromSet(fields.Set{"status.phase": "Running"}),
	})
	if err != nil {
		return err
	}

	runningPodIPs := []string{}

	for _, pod := range pods.Items {
		node := &corev1.Node{}
		err := r.Get(context.TODO(), types.NamespacedName{
			Name: pod.Spec.NodeName,
		}, node)
		if err == nil && isNodeReady(node) {
			runningPodIPs = append(runningPodIPs, pod.Status.PodIP)
		}
	}

	if len(runningPodIPs) != 1 {
		return fmt.Errorf("expected 1 controller pod got %d", len(pods.Items))
	}
	podIPMap[cr.Name] = runningPodIPs[0]

	return nil
}

func isNodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			if cond.Status == corev1.ConditionTrue {
				return true
			}
		}
	}
	return false
}

func (r *JivaVolumeReconciler) isScaleup(cr *openebsiov1alpha1.JivaVolume) bool {
	if cr.Spec.DesiredReplicationFactor > cr.Spec.Policy.Target.ReplicationFactor {
		if cr.Spec.Policy.Target.ReplicationFactor != cr.Status.ReplicaCount {
			r.Recorder.Eventf(cr, corev1.EventTypeWarning,
				"ReplicaScaleup", "failed to scaleup volume, replica count: %v in status not equal to replicationfactor: %v",
				cr.Status.ReplicaCount, cr.Spec.Policy.Target.ReplicationFactor)
			logrus.Errorf("failed to scaleup, replica count: %v in status not equal to replicationfactor: %v",
				cr.Status.ReplicaCount, cr.Spec.Policy.Target.ReplicationFactor)
			return false
		}
		for _, rep := range cr.Status.ReplicaStatuses {
			if rep.Mode != "RW" {
				r.Recorder.Eventf(cr, corev1.EventTypeWarning,
					"ReplicaScaleup", "failed to scaleup volume, all replicas for volume %v should be in RW state", cr.Name)
				logrus.Errorf("failed to scaleup, all replicas for volume %v should be in RW state", cr.Name)
				return false
			}
		}
		if cr.Spec.DesiredReplicationFactor-cr.Spec.Policy.Target.ReplicationFactor != 1 {
			r.Recorder.Eventf(cr, corev1.EventTypeWarning,
				"ReplicaScaleup", "failed to scaleup volume, only single replica scaleup is allowed, desired: %v actual: %v",
				cr.Spec.DesiredReplicationFactor, cr.Spec.Policy.Target.ReplicationFactor)
			logrus.Errorf("failed to scaleup, only single replica scaleup is allowed, desired: %v actual: %v",
				cr.Spec.DesiredReplicationFactor, cr.Spec.Policy.Target.ReplicationFactor)
			return false
		}
		return true
	}
	return false
}

// isHAVolume checks if the volume has atleast
// qurom number of replicas in RW state
func isHAVolume(cr *openebsiov1alpha1.JivaVolume) bool {
	if cr.Spec.Policy.Target.ReplicationFactor < 3 {
		return false
	}
	availableReplicas := 0
	qurom := (cr.Spec.Policy.Target.ReplicationFactor / 2) + 1
	for _, rep := range cr.Status.ReplicaStatuses {
		if rep.Mode == "RW" {
			availableReplicas += 1
			if availableReplicas == qurom {
				return true
			}
		}
	}
	return false
}

func (r *JivaVolumeReconciler) moveReplicasForMissingNodes(cr *openebsiov1alpha1.JivaVolume) error {

	// if the volume does not HA replicas in
	// RW mode skip the process
	if !isHAVolume(cr) {
		return nil
	}

	var (
		replicaLabel   = "openebs.io/component=jiva-replica,openebs.io/persistent-volume="
		nodeAnnotation = "volume.kubernetes.io/selected-node"
	)
	labelSelector, err := labels.Parse(
		replicaLabel + cr.Name)
	if err != nil {
		return err
	}
	pods := corev1.PodList{}
	err = r.List(context.TODO(), &pods, &client.ListOptions{
		Namespace:     cr.Namespace,
		LabelSelector: labelSelector,
	})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		// perform steps only if the pod is in pending state
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		err = r.Get(context.TODO(),
			types.NamespacedName{
				Name:      pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName,
				Namespace: pod.Namespace,
			}, pvc)
		if err != nil {
			// if the PVC is missing then only
			// delete the sts pod
			if errors.IsNotFound(err) {
				err = r.Delete(context.TODO(), &pod)
				// wait for pod to get deleted and
				// recreated
				time.Sleep(10 * time.Second)
				if err != nil && !errors.IsNotFound(err) {
					return err
				}
				continue
			}
			return err
		}
		nodeName := pvc.GetAnnotations()[nodeAnnotation]
		// if a pvc and pod is deleted then in next iteration the nodeName
		// will be empty which will end up in not-found error
		// this can result in a race between pvc getting bound and operator deleting
		// the pending pvc, so performing steps only if nodeName is present
		if nodeName != "" {
			err = r.Get(context.TODO(), types.NamespacedName{
				Name: nodeName,
			}, &corev1.Node{})
			if err != nil {
				if errors.IsNotFound(err) {
					err = r.removeSTSVolume(pvc)
					if err != nil {
						return err
					}
					err = r.Delete(context.TODO(), &pod)
					if err != nil {
						return err
					}
					// wait for pod to get deleted and
					// recreated
					time.Sleep(10 * time.Second)
					r.Recorder.Eventf(cr, corev1.EventTypeWarning,
						"ReplicaMovement",
						"replica %s and it's corresponding PVC & PV deleted",
						pod.Name,
					)
				} else {
					return err
				}
			}
		}
	}
	return nil
}

// remove the stale PVC and PV for the missing node
func (r *JivaVolumeReconciler) removeSTSVolume(pvc *corev1.PersistentVolumeClaim) error {
	pv := &corev1.PersistentVolume{}
	newPV := &corev1.PersistentVolume{}
	err := r.Get(context.TODO(),
		types.NamespacedName{Name: pvc.Spec.VolumeName}, pv)
	if err != nil {
		// if PV is not found skip over to PVC deletion
		if errors.IsNotFound(err) {
			goto deletepvc
		}
		return err
	}
	newPV = pv.DeepCopy()
	newPV.ObjectMeta.Finalizers = []string{}
	err = r.Patch(context.TODO(), newPV, client.MergeFrom(pv))
	if err != nil {
		return err
	}
	err = r.Delete(context.TODO(), pv)
	if err != nil {
		return err
	}

deletepvc:

	newPVC := pvc.DeepCopy()
	newPVC.ObjectMeta.Finalizers = []string{}
	err = r.Patch(context.TODO(), newPVC, client.MergeFrom(pvc))
	if err != nil {
		return err
	}

	err = r.Delete(context.TODO(), pvc)
	if err != nil {
		return err
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *JivaVolumeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.TODO(), &corev1.Pod{}, "status.phase", func(rawObj client.Object) []string {
		pod := rawObj.(*corev1.Pod)
		return []string{string(pod.Status.Phase)}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&openebsiov1alpha1.JivaVolume{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&appsv1.StatefulSet{}).
		Complete(r)
}

func (r *JivaVolumeReconciler) finally(err error, cr *openebsiov1alpha1.JivaVolume) {
	if err != nil {
		cr.Status.Phase = openebsiov1alpha1.JivaVolumePhaseFailed
		logrus.Errorf("failed to bootstrap volume %s, due to error: %v", cr.Name, err)
	} else {
		cr.Status.Phase = openebsiov1alpha1.JivaVolumePhaseSyncing
	}

	if err := r.updateJivaVolume(cr); err != nil {
		logrus.Error(err, "failed to update JivaVolume phase")
	}
}

func (r *JivaVolumeReconciler) shouldReconcile(cr *openebsiov1alpha1.JivaVolume) (bool, error) {
	operatorVersion := version.Version
	jivaVolumeVersion := cr.VersionDetails.Status.Current

	if jivaVolumeVersion != operatorVersion {
		return false, fmt.Errorf("jiva operator version is %s but volume %s version is %s",
			operatorVersion, cr.Name, jivaVolumeVersion)
	}

	return true, nil
}

// 1. Create controller svc
// 2. Create controller deploy
// 3. Create replica statefulset
func (r *JivaVolumeReconciler) bootstrapJiva(cr *openebsiov1alpha1.JivaVolume) (err error) {
	for _, f := range installFuncs {
		if err = f(r, cr); err != nil {
			r.Recorder.Eventf(cr, corev1.EventTypeWarning,
				"Bootstrap", "failed to bootstrap volume, due to error: %v", err)
			break
		}
	}
	r.finally(err, cr)
	return err
}

// TODO: add logic to create disruption budget for replicas
func createReplicaPodDisruptionBudget(r *JivaVolumeReconciler, cr *openebsiov1alpha1.JivaVolume) error {
	min := cr.Spec.Policy.Target.ReplicationFactor
	pdbObj := &policyv1beta1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PodDisruptionBudget",
			APIVersion: pdbAPIVersion,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-pdb",
			Namespace: cr.Namespace,
		},
		Spec: policyv1beta1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: defaultReplicaMatchLabels(cr.Spec.PV),
			},
			MinAvailable: &intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: int32(min/2 + 1),
			},
		},
	}

	instance := &policyv1beta1.PodDisruptionBudget{}
	err := r.Get(context.TODO(), types.NamespacedName{Name: pdbObj.Name, Namespace: pdbObj.Namespace}, instance)
	if err != nil && errors.IsNotFound(err) {
		// Set JivaVolume instance as the owner and controller
		if err := controllerutil.SetControllerReference(cr, pdbObj, r.Scheme); err != nil {
			return err
		}

		logrus.Info("Creating a new pod disruption budget", "Pdb.Namespace", pdbObj.Namespace, "Pdb.Name", pdbObj.Name)
		err = r.Create(context.TODO(), pdbObj)
		if err != nil {
			return err
		}
		// pdb created successfully - don't requeue
		return nil
	} else if err != nil {
		return operr.Wrapf(err, "failed to get the pod disruption budget details: %v", pdbObj.Name)
	}

	return nil
}

func (r *JivaVolumeReconciler) performScaleup(cr *openebsiov1alpha1.JivaVolume) error {
	// update the replica sts with the desired replica count
	// this will bring a new hostpath pvc on a new node and a
	// new pod
	replicaName := cr.Name + "-jiva-rep"
	replicaSTS := &appsv1.StatefulSet{}
	err := r.Get(context.TODO(),
		types.NamespacedName{Name: replicaName, Namespace: cr.Namespace}, replicaSTS)
	if err != nil {
		return err
	}
	desiredReplicas := int32(cr.Spec.DesiredReplicationFactor)
	newReplicaSTS := replicaSTS.DeepCopy()
	newReplicaSTS.Spec.Replicas = &desiredReplicas
	err = r.Patch(context.TODO(), newReplicaSTS, client.MergeFrom(replicaSTS))
	if err != nil {
		return err
	}

	// update the controller envs to the desired replica count
	controllerName := cr.Name + "-jiva-ctrl"
	ctrlDeploy := &appsv1.Deployment{}
	err = r.Get(context.TODO(),
		types.NamespacedName{Name: controllerName, Namespace: cr.Namespace}, ctrlDeploy)
	if err != nil {
		return err
	}
	newCtrlDeploy := ctrlDeploy.DeepCopy()
	for i, con := range newCtrlDeploy.Spec.Template.Spec.Containers {
		if con.Name == "jiva-controller" {
			newCtrlDeploy.Spec.Template.Spec.Containers[i].Env[0].Value = strconv.Itoa(cr.Spec.DesiredReplicationFactor)
		}
	}
	err = r.Patch(context.TODO(), newCtrlDeploy, client.MergeFrom(ctrlDeploy))
	if err != nil {
		return err
	}

	cr.Spec.Policy.Target.ReplicationFactor = int(desiredReplicas)
	cr.Status.Phase = openebsiov1alpha1.JivaVolumePhaseSyncing
	if err := r.updateJivaVolume(cr); err != nil {
		return fmt.Errorf("failed to update JivaVolume phase: %s", err.Error())
	}
	return nil
}

func createControllerDeployment(r *JivaVolumeReconciler, cr *openebsiov1alpha1.JivaVolume) error {
	reps := int32(1)

	dep, err := deploy.NewBuilder().WithName(cr.Name + "-jiva-ctrl").
		WithNamespace(cr.Namespace).
		WithLabels(defaultControllerLabels(cr.Spec.PV, cr.GetLabels()[openebsPVC])).
		WithReplicas(&reps).
		WithStrategyType(appsv1.RecreateDeploymentStrategyType).
		WithSelectorMatchLabelsNew(defaultControllerMatchLabels(cr.Spec.PV, cr.GetLabels()[openebsPVC])).
		WithPodTemplateSpecBuilder(
			func() *pts.Builder {
				ptsBuilder := pts.NewBuilder().
					WithLabels(defaultControllerLabels(cr.Spec.PV, cr.GetLabels()[openebsPVC])).
					WithServiceAccountName(defaultServiceAccountName).
					WithAnnotations(defaultAnnotations()).
					WithTolerations(cr.Spec.Policy.Target.Tolerations...).
					WithContainerBuilders(
						container.NewBuilder().
							WithName("jiva-controller").
							WithImage(getImage("OPENEBS_IO_JIVA_CONTROLLER_IMAGE",
								"jiva-controller")).
							WithPortsNew(defaultControllerPorts()).
							WithCommandNew([]string{
								"launch",
							}).
							WithArgumentsNew([]string{
								"controller",
								"--frontend",
								"gotgt",
								"--clusterIP",
								cr.Spec.ISCSISpec.TargetIP,
								cr.Name,
							}).
							WithEnvsNew([]corev1.EnvVar{
								{
									Name:  "REPLICATION_FACTOR",
									Value: strconv.Itoa(cr.Spec.Policy.Target.ReplicationFactor),
								},
							}).
							WithResources(cr.Spec.Policy.Target.Resources).
							WithImagePullPolicy(corev1.PullIfNotPresent),
					)
				if !cr.Spec.Policy.Target.DisableMonitor {
					ptsBuilder = ptsBuilder.WithContainerBuilders(
						container.NewBuilder().
							WithImage(getImage("OPENEBS_IO_MAYA_EXPORTER_IMAGE",
								"exporter")).
							WithImagePullPolicy(corev1.PullIfNotPresent).
							WithName("maya-volume-exporter").
							WithCommandNew([]string{"maya-exporter"}).
							WithPortsNew([]corev1.ContainerPort{
								{
									ContainerPort: 9500,
									Protocol:      "TCP",
								},
							},
							).
							WithResources(cr.Spec.Policy.Target.AuxResources),
					)
				}
				if len(cr.Spec.Policy.ServiceAccountName) != 0 {
					ptsBuilder = ptsBuilder.WithServiceAccountName(cr.Spec.Policy.ServiceAccountName)
				}
				if len(cr.Spec.Policy.PriorityClassName) != 0 {
					ptsBuilder = ptsBuilder.WithPriorityClassName(cr.Spec.Policy.PriorityClassName)
				}
				if cr.Spec.Policy.Target.NodeSelector != nil {
					ptsBuilder = ptsBuilder.WithNodeSelector(cr.Spec.Policy.Target.NodeSelector)
				}
				if cr.Spec.Policy.Target.Affinity != nil {
					ptsBuilder = ptsBuilder.WithAffinity(cr.Spec.Policy.Target.Affinity)
				}
				return ptsBuilder
			}(),
		).Build()

	if err != nil {
		return fmt.Errorf("failed to build deployment object, err: %v", err)
	}

	instance := &appsv1.Deployment{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: dep.Name, Namespace: dep.Namespace}, instance)
	if err != nil && errors.IsNotFound(err) {
		// Set JivaVolume instance as the owner and controller
		if err := controllerutil.SetControllerReference(cr, dep, r.Scheme); err != nil {
			return err
		}

		logrus.Info("Creating a new deployment", "Deploy.Namespace", dep.Namespace, "Deploy.Name", dep.Name)
		err = r.Create(context.TODO(), dep)
		if err != nil {
			return err
		}
		// deployment created successfully - don't requeue
		return nil
	} else if err != nil {
		return operr.Wrapf(err, "failed to get the deployment details: %v", dep.Name)
	}

	return nil
}

func getImage(key, component string) string {
	image, present := os.LookupEnv(key)
	if !present {
		switch component {
		case "jiva-controller", "jiva-replica":
			image = "openebs/jiva:ci"
		case "exporter":
			image = "openebs/m-exporter:ci"
		}
	}
	return image
}

func defaultReplicaLabels(pv string) map[string]string {
	labels := defaultReplicaMatchLabels(pv)
	labels["openebs.io/version"] = version.Version
	return labels
}

func defaultReplicaMatchLabels(pv string) map[string]string {
	return map[string]string{
		"openebs.io/cas-type":          "jiva",
		"openebs.io/component":         "jiva-replica",
		"openebs.io/persistent-volume": pv,
	}
}

func defaultControllerLabels(pv string, pvc string) map[string]string {
	labels := defaultControllerMatchLabels(pv, pvc)
	labels["openebs.io/version"] = version.Version
	return labels
}

func defaultControllerMatchLabels(pv string, pvc string) map[string]string {
	return map[string]string{
		"openebs.io/cas-type":          "jiva",
		"openebs.io/component":         "jiva-controller",
		"openebs.io/persistent-volume": pv,
		openebsPVC:                     pvc,
	}
}

func defaultAnnotations() map[string]string {
	return map[string]string{"prometheus.io/path": "/metrics",
		"prometheus.io/port":  "9500",
		"prometheus.io/scrap": "true",
	}
}

func defaultControllerPorts() []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{
			ContainerPort: 3260,
			Protocol:      "TCP",
		},
		{
			ContainerPort: 9501,
			Protocol:      "TCP",
		},
	}
}

func defaultControllerSVCPorts() []corev1.ServicePort {
	return []corev1.ServicePort{
		{
			Name:       "iscsi",
			Port:       3260,
			Protocol:   "TCP",
			TargetPort: intstr.IntOrString{IntVal: 3260},
		},
		{
			Name:       "api",
			Port:       9501,
			Protocol:   "TCP",
			TargetPort: intstr.IntOrString{IntVal: 9501},
		},
		{
			Name:       "exporter",
			Port:       9500,
			Protocol:   "TCP",
			TargetPort: intstr.IntOrString{IntVal: 9500},
		},
	}
}

func defaultReplicaPorts() []corev1.ContainerPort {
	return []corev1.ContainerPort{
		{
			ContainerPort: 9502,
			Protocol:      "TCP",
		},
		{
			ContainerPort: 9503,
			Protocol:      "TCP",
		},
		{
			ContainerPort: 9504,
			Protocol:      "TCP",
		},
	}
}

func defaultServiceLabels(pv string) map[string]string {
	return map[string]string{
		"openebs.io/cas-type":          "jiva",
		"openebs.io/component":         "jiva-controller-service",
		"openebs.io/persistent-volume": pv,
		"openebs.io/version":           version.Version,
	}
}

func createReplicaStatefulSet(r *JivaVolumeReconciler, cr *openebsiov1alpha1.JivaVolume) error {

	var (
		err                            error
		replicaCount                   int32
		stsObj                         *appsv1.StatefulSet
		blockOwnerDeletion, controller = false, true
		svcName                        = cr.Name + "-jiva-ctrl-svc"
	)

	svc := &corev1.Service{}
	err = r.Get(context.TODO(),
		types.NamespacedName{
			Name:      svcName,
			Namespace: cr.Namespace,
		},
		svc)
	if err != nil {
		return fmt.Errorf("failed to get svc %s, err: %v", svcName, err)
	}

	rc := cr.Spec.Policy.Target.ReplicationFactor
	replicaCount = int32(rc)
	prev := true

	size := strings.Split(cr.Spec.Capacity, "i")[0]
	capacity, err := units.RAMInBytes(size)
	if err != nil {
		return fmt.Errorf("failed to convert human readable size: %v into int64, err: %v", cr.Spec.Capacity, err)
	}

	defaultLabels := defaultReplicaLabels(cr.Spec.PV)

	stsObj, err = sts.NewBuilder().
		WithName(cr.Name + "-jiva-rep").
		WithLabelsNew(defaultReplicaLabels(cr.Spec.PV)).
		WithNamespace(cr.Namespace).
		WithServiceName("jiva-replica-svc").
		WithPodManagementPolicy(appsv1.ParallelPodManagement).
		WithStrategyType(appsv1.RollingUpdateStatefulSetStrategyType).
		WithReplicas(&replicaCount).
		WithSelectorMatchLabels(defaultReplicaMatchLabels(cr.Spec.PV)).
		WithPodTemplateSpecBuilder(
			func() *pts.Builder {
				ptsBuilder := pts.NewBuilder().
					//WithLabels(defaultReplicaLabels(cr.Spec.PV)).
					WithServiceAccountName(defaultServiceAccountName).
					WithContainerBuilders(
						container.NewBuilder().
							WithName("jiva-replica").
							WithImage(getImage("OPENEBS_IO_JIVA_REPLICA_IMAGE",
								"jiva-replica")).
							WithPortsNew(defaultReplicaPorts()).
							WithCommandNew([]string{
								"launch",
							}).
							WithArgumentsNew([]string{
								"replica",
								"--frontendIP",
								svc.Spec.ClusterIP,
								"--size",
								fmt.Sprint(capacity),
								"openebs",
							}).
							WithImagePullPolicy(corev1.PullIfNotPresent).
							WithPrivilegedSecurityContext(&prev).
							WithResources(cr.Spec.Policy.Replica.Resources).
							WithVolumeMountsNew([]corev1.VolumeMount{
								{
									Name:      "openebs",
									MountPath: "/openebs",
								},
							}),
					).
					WithTolerations(cr.Spec.Policy.Replica.Tolerations...)
				if len(cr.Spec.Policy.ServiceAccountName) != 0 {
					ptsBuilder = ptsBuilder.WithServiceAccountName(cr.Spec.Policy.ServiceAccountName)
				}
				if len(cr.Spec.Policy.PriorityClassName) != 0 {
					ptsBuilder = ptsBuilder.WithPriorityClassName(cr.Spec.Policy.PriorityClassName)
				}
				if cr.Spec.Policy.Replica.NodeSelector != nil {
					ptsBuilder = ptsBuilder.WithNodeSelector(cr.Spec.Policy.Replica.NodeSelector)
				}
				if cr.Spec.Policy.Replica.Affinity != nil {
					if cr.Spec.Policy.Replica.Affinity.PodAntiAffinity != nil {
						for _, term := range cr.Spec.Policy.Replica.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
							selectorMap, _ = metav1.LabelSelectorAsMap(term.LabelSelector)
						}
						defaultLabels[replicaAntiAffinityKey] = selectorMap[replicaAntiAffinityKey]
					}
				}
				ptsBuilder = ptsBuilder.WithLabels(defaultLabels)
				affinity := &corev1.Affinity{
					PodAntiAffinity: &corev1.PodAntiAffinity{
						RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{
							{
								LabelSelector: &metav1.LabelSelector{
									MatchLabels: defaultReplicaMatchLabels(cr.Spec.PV),
								},
								TopologyKey: "kubernetes.io/hostname",
							},
						},
					},
				}

				// update any affinities has been configured using jiva volume policy
				if cr.Spec.Policy.Replica.Affinity != nil {
					if cr.Spec.Policy.Replica.Affinity.PodAntiAffinity != nil {
						affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution = append(
							affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
							cr.Spec.Policy.Replica.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution...,
						)
					}
					affinity.NodeAffinity = cr.Spec.Policy.Replica.Affinity.NodeAffinity
					affinity.PodAffinity = cr.Spec.Policy.Replica.Affinity.PodAffinity
				}

				ptsBuilder = ptsBuilder.WithAffinity(affinity)

				return ptsBuilder
			}(),
		).
		WithPVC(
			pvc.NewBuilder().
				WithName("openebs").
				WithNamespace(cr.Namespace).
				WithOwnerReferenceNew([]metav1.OwnerReference{{
					APIVersion:         cr.APIVersion,
					BlockOwnerDeletion: &blockOwnerDeletion,
					Controller:         &controller,
					Kind:               cr.Kind,
					Name:               cr.Name,
					UID:                cr.UID,
				},
				}).
				WithStorageClass(cr.Spec.Policy.ReplicaSC).
				WithAccessModes([]corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}).
				WithCapacity(cr.Spec.Capacity),
		).Build()

	if err != nil {
		return fmt.Errorf("failed to build statefulset object, err: %v", err)
	}

	instance := &appsv1.StatefulSet{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: stsObj.Name, Namespace: stsObj.Namespace}, instance)
	if err != nil && errors.IsNotFound(err) {
		// Set JivaVolume instance as the owner and controller
		if err := controllerutil.SetControllerReference(cr, stsObj, r.Scheme); err != nil {
			return err
		}

		logrus.Info("Creating a new Statefulset", "Statefulset.Namespace", stsObj.Namespace, "Sts.Name", stsObj.Name)
		err = r.Create(context.TODO(), stsObj)
		if err != nil {
			return err
		}
		// Statefulset created successfully - don't requeue
		return nil
	} else if err != nil {
		return operr.Wrapf(err, "failed to get the statefulset details: %v", stsObj.Name)
	}

	return nil
}

func updateJivaVolumeWithServiceInfo(r *JivaVolumeReconciler, cr *openebsiov1alpha1.JivaVolume) error {
	ctrlSVC := &corev1.Service{}
	if err := r.Get(context.TODO(),
		types.NamespacedName{
			Name:      cr.Name + "-jiva-ctrl-svc",
			Namespace: cr.Namespace,
		}, ctrlSVC); err != nil {
		return fmt.Errorf("%s, err: %v", updateErrMsg, err)
	}
	cr.Spec.ISCSISpec.TargetIP = ctrlSVC.Spec.ClusterIP
	var found bool
	for _, port := range ctrlSVC.Spec.Ports {
		if port.Name == "iscsi" {
			found = true
			cr.Spec.ISCSISpec.TargetPort = port.Port
			cr.Spec.ISCSISpec.Iqn = "iqn.2016-09.com.openebs.jiva" + ":" + cr.Spec.PV
		}
	}

	if !found {
		return fmt.Errorf("%s, err: can't find targetPort in target service spec: {%+v}", updateErrMsg, ctrlSVC)
	}

	logrus.Info("Updating JivaVolume with iscsi spec", "ISCSISpec", cr.Spec.ISCSISpec)
	cr.Status.Phase = openebsiov1alpha1.JivaVolumePhasePending
	if err := r.Update(context.TODO(), cr); err != nil {
		return fmt.Errorf("%s, err: %v", updateErrMsg, err)
	}

	// Update cr with the updated fields so that we don't get
	// resourceVersion changed error in next steps
	if err := r.getJivaVolume(cr); err != nil {
		return fmt.Errorf("%s, err: %v", updateErrMsg, err)
	}

	return nil
}

func getBaseReplicaTolerations() []corev1.Toleration {
	return []corev1.Toleration{
		corev1.Toleration{
			Key:      "node.kubernetes.io/notReady",
			Effect:   corev1.TaintEffectNoExecute,
			Operator: corev1.TolerationOpExists,
		},
		corev1.Toleration{
			Key:      "node.cloudprovider.kubernetes.io/uninitialized",
			Effect:   corev1.TaintEffectNoExecute,
			Operator: corev1.TolerationOpExists,
		},
		corev1.Toleration{
			Key:      "node.kubernetes.io/unreachable",
			Effect:   corev1.TaintEffectNoExecute,
			Operator: corev1.TolerationOpExists,
		},
		corev1.Toleration{
			Key:      "node.kubernetes.io/not-ready",
			Effect:   corev1.TaintEffectNoExecute,
			Operator: corev1.TolerationOpExists,
		},
		corev1.Toleration{
			Key:      "node.kubernetes.io/unschedulable",
			Effect:   corev1.TaintEffectNoExecute,
			Operator: corev1.TolerationOpExists,
		},
		corev1.Toleration{
			Key:      "node.kubernetes.io/out-of-disk",
			Effect:   corev1.TaintEffectNoExecute,
			Operator: corev1.TolerationOpExists,
		},
		corev1.Toleration{
			Key:      "node.kubernetes.io/memory-pressure",
			Effect:   corev1.TaintEffectNoExecute,
			Operator: corev1.TolerationOpExists,
		},
		corev1.Toleration{
			Key:      "node.kubernetes.io/disk-pressure",
			Effect:   corev1.TaintEffectNoExecute,
			Operator: corev1.TolerationOpExists,
		},
		corev1.Toleration{
			Key:      "node.kubernetes.io/network-unavailable",
			Effect:   corev1.TaintEffectNoExecute,
			Operator: corev1.TolerationOpExists,
		},
	}
}

func getBaseTargetTolerations() []corev1.Toleration {
	var zero int64
	return []corev1.Toleration{
		corev1.Toleration{
			Key:               "node.kubernetes.io/notReady",
			Effect:            corev1.TaintEffectNoExecute,
			Operator:          corev1.TolerationOpExists,
			TolerationSeconds: &zero,
		},
		corev1.Toleration{
			Key:               "node.kubernetes.io/unreachable",
			Effect:            corev1.TaintEffectNoExecute,
			Operator:          corev1.TolerationOpExists,
			TolerationSeconds: &zero,
		},
		corev1.Toleration{
			Key:               "node.kubernetes.io/not-ready",
			Effect:            corev1.TaintEffectNoExecute,
			Operator:          corev1.TolerationOpExists,
			TolerationSeconds: &zero,
		},
	}
}

// getDefaultPolicySpec gives the default policy spec for jiva volume.
func getDefaultPolicySpec() openebsiov1alpha1.JivaVolumePolicySpec {
	return openebsiov1alpha1.JivaVolumePolicySpec{
		ReplicaSC: defaultStorageClass,
		Target: openebsiov1alpha1.TargetSpec{
			PodTemplateResources: openebsiov1alpha1.PodTemplateResources{
				Tolerations: getBaseTargetTolerations(),
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("0"),
						corev1.ResourceMemory: resource.MustParse("0"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("0"),
						corev1.ResourceMemory: resource.MustParse("0"),
					},
				},
			},
			AuxResources: &corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("0"),
					corev1.ResourceMemory: resource.MustParse("0"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("0"),
					corev1.ResourceMemory: resource.MustParse("0"),
				},
			},
			ReplicationFactor: defaultReplicationFactor,
			DisableMonitor:    defaultDisableMonitor,
		},
		Replica: openebsiov1alpha1.ReplicaSpec{
			PodTemplateResources: openebsiov1alpha1.PodTemplateResources{
				Tolerations: getBaseReplicaTolerations(),
				Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("0"),
						corev1.ResourceMemory: resource.MustParse("0"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("0"),
						corev1.ResourceMemory: resource.MustParse("0"),
					},
				},
			},
		},
	}
}

func defaultRF(policy *openebsiov1alpha1.JivaVolumePolicySpec, defaultPolicy openebsiov1alpha1.JivaVolumePolicySpec) {
	if policy.Target.ReplicationFactor == 0 {
		policy.Target.ReplicationFactor = defaultPolicy.Target.ReplicationFactor
	}
}

func defaultSC(policy *openebsiov1alpha1.JivaVolumePolicySpec, defaultPolicy openebsiov1alpha1.JivaVolumePolicySpec) {
	if policy.ReplicaSC == "" {
		policy.ReplicaSC = defaultPolicy.ReplicaSC
	}
}

func defaultTargetRes(policy *openebsiov1alpha1.JivaVolumePolicySpec, defaultPolicy openebsiov1alpha1.JivaVolumePolicySpec) {
	if policy.Target.Resources == nil {
		policy.Target.Resources = defaultPolicy.Target.Resources
	}
}

func defaultTargetAuxRes(policy *openebsiov1alpha1.JivaVolumePolicySpec, defaultPolicy openebsiov1alpha1.JivaVolumePolicySpec) {
	if policy.Target.AuxResources == nil {
		policy.Target.AuxResources = defaultPolicy.Target.AuxResources
	}
}

func defaultReplicaRes(policy *openebsiov1alpha1.JivaVolumePolicySpec, defaultPolicy openebsiov1alpha1.JivaVolumePolicySpec) {
	if policy.Replica.Resources == nil {
		policy.Replica.Resources = defaultPolicy.Replica.Resources
	}
}

func defaultTargetTolerations(policy *openebsiov1alpha1.JivaVolumePolicySpec, defaultPolicy openebsiov1alpha1.JivaVolumePolicySpec) {
	policy.Target.Tolerations = append(defaultPolicy.Target.Tolerations, policy.Target.Tolerations...)
}

func defaultReplicaTolerations(policy *openebsiov1alpha1.JivaVolumePolicySpec, defaultPolicy openebsiov1alpha1.JivaVolumePolicySpec) {
	policy.Replica.Tolerations = append(defaultPolicy.Replica.Tolerations, policy.Replica.Tolerations...)
}

// validatePolicySpec checks the policy provided by the user and sets the
// defaults to the policy spec of jiva volume.
func validatePolicySpec(policy *openebsiov1alpha1.JivaVolumePolicySpec) {
	defaultPolicy := getDefaultPolicySpec()
	optFuncs := []policyOptFuncs{
		defaultRF, defaultSC, defaultTargetRes, defaultReplicaRes,
		defaultTargetTolerations, defaultReplicaTolerations,
		defaultTargetAuxRes,
	}
	for _, o := range optFuncs {
		o(policy, defaultPolicy)
	}
}

func populateJivaVolumePolicy(r *JivaVolumeReconciler, cr *openebsiov1alpha1.JivaVolume) error {
	policyName := cr.Annotations["openebs.io/volume-policy"]
	policySpec := getDefaultPolicySpec()
	// if policy name is provided via annotation get and validate the
	// policy spec else set the default policy spec.
	if policyName != "" {
		policy := openebsiov1alpha1.JivaVolumePolicy{}
		err := r.Get(
			context.TODO(),
			types.NamespacedName{Name: policyName, Namespace: cr.Namespace},
			&policy,
		)
		if err != nil {
			return operr.Wrapf(err, "failed to get volume policy %s", policyName)
		}
		policySpec = policy.Spec
		validatePolicySpec(&policySpec)
	}
	cr.Spec.Policy = policySpec
	cr.Spec.DesiredReplicationFactor = policySpec.Target.ReplicationFactor
	return nil
}

func createControllerService(r *JivaVolumeReconciler, cr *openebsiov1alpha1.JivaVolume) error {

	// By default type is clusterIP
	svcObj, err := svc.NewBuilder().
		WithName(cr.Name + "-jiva-ctrl-svc").
		WithLabelsNew(defaultServiceLabels(cr.Spec.PV)).
		WithNamespace(cr.Namespace).
		WithSelectorsNew(map[string]string{
			"openebs.io/cas-type":          "jiva",
			"openebs.io/persistent-volume": cr.Spec.PV,
		}).
		WithPorts(defaultControllerSVCPorts()).
		Build()

	if err != nil {
		return fmt.Errorf("failed to build service object, err: %v", err)
	}

	instance := &corev1.Service{}
	err = r.Get(context.TODO(), types.NamespacedName{Name: svcObj.Name, Namespace: svcObj.Namespace}, instance)
	if err != nil && errors.IsNotFound(err) {
		// Set JivaVolume instance as the owner and controller
		if err := controllerutil.SetControllerReference(cr, svcObj, r.Scheme); err != nil {
			return err
		}

		logrus.Info("Creating a new service", "Service.Namespace", svcObj.Namespace, "Service.Name", svcObj.Name)
		err = r.Create(context.TODO(), svcObj)
		if err != nil {
			return err
		}
		// Wait for service to get created
		time.Sleep(1 * time.Second)
		return updateJivaVolumeWithServiceInfo(r, cr)
	} else if err != nil {
		return operr.Wrapf(err, "failed to get the service details: %v", svcObj.Name)

	}

	return updateJivaVolumeWithServiceInfo(r, cr)

}

func (r *JivaVolumeReconciler) updateJivaVolume(cr *openebsiov1alpha1.JivaVolume) error {
	if err := r.Update(context.TODO(), cr); err != nil {
		return fmt.Errorf("failed to update JivaVolume, err: %v", err)
	}
	if err := r.getJivaVolume(cr); err != nil {
		return fmt.Errorf("failed to get JivaVolume, err: %v", err)
	}

	return nil
}

func (r *JivaVolumeReconciler) getJivaVolume(cr *openebsiov1alpha1.JivaVolume) error {
	instance := &openebsiov1alpha1.JivaVolume{}
	if err := r.Get(context.TODO(),
		types.NamespacedName{
			Name:      cr.Name,
			Namespace: cr.Namespace,
		}, instance); err != nil {
		return err
	}

	// update cr with the latest change
	cr = instance.DeepCopy()
	return nil
}

// setdefaults set the default value
func setdefaults(cr *openebsiov1alpha1.JivaVolume) {
	cr.Status = openebsiov1alpha1.JivaVolumeStatus{
		Status: "Unknown",
		Phase:  openebsiov1alpha1.JivaVolumePhaseSyncing,
	}
}

func (r *JivaVolumeReconciler) updateStatus(err *error, cr *openebsiov1alpha1.JivaVolume) {
	if *err != nil {
		setdefaults(cr)
	}
	if err := r.updateJivaVolume(cr); err != nil {
		logrus.Error(err, "failed to update status")
	}
	if err := r.getJivaVolume(cr); err != nil {
		logrus.Error(err, "failed to get JivaVolume")
	}
}

func (r *JivaVolumeReconciler) getAndUpdateVolumeStatus(cr *openebsiov1alpha1.JivaVolume) error {
	var (
		cli *jiva.ControllerClient
		err error
	)

	defer r.updateStatus(&err, cr)

	if err = r.getJivaVolume(cr); err != nil {
		return fmt.Errorf("failed to getAndUpdateVolumeStatus, err: %v", err)
	}

	addr := cr.Spec.ISCSISpec.TargetIP + ":9501"
	if podIP, ok := podIPMap[cr.Name]; ok {
		addr = podIP + ":9501"
	}

	if len(addr) == 0 {
		return fmt.Errorf("failed to get volume stats: target address is empty")
	}

	cli = jiva.NewControllerClient(addr)
	stats := &volume.Stats{}
	err = cli.Get("/stats", stats)
	if err != nil {
		// log err only, as controller must be in container creating state
		// don't return err as it will dump stack trace unneccesary
		logrus.Info("failed to get volume stats ", "err", err)
		err = r.updatePodIPMap(cr)
		if err != nil {
			logrus.Infof("failed to get controller pod ip for volume %s: %s", cr.Name, err.Error())
			time.Sleep(1 * time.Second)
		}
	}

	cr.Status.Status = stats.TargetStatus
	cr.Status.ReplicaCount = len(stats.Replicas)
	cr.Status.ReplicaStatuses = make([]openebsiov1alpha1.ReplicaStatus, len(stats.Replicas))

	for i, rep := range stats.Replicas {
		cr.Status.ReplicaStatuses[i].Address = rep.Address
		cr.Status.ReplicaStatuses[i].Mode = rep.Mode
	}

	if stats.TargetStatus == "RW" {
		cr.Status.Phase = openebsiov1alpha1.JivaVolumePhaseReady
	} else if stats.TargetStatus == "RO" {
		cr.Status.Phase = openebsiov1alpha1.JivaVolumePhaseSyncing
	} else {
		cr.Status.Phase = openebsiov1alpha1.JivaVolumePhaseUnkown
	}

	return nil
}

func (r *JivaVolumeReconciler) reconcileVersion(cr *openebsiov1alpha1.JivaVolume) error {
	var err error
	// the below code uses deep copy to have the state of object just before
	// any update call is done so that on failure the last state object can be returned
	if cr.VersionDetails.Status.Current != cr.VersionDetails.Desired {
		if !version.IsCurrentVersionValid(cr.VersionDetails.Status.Current) {
			return fmt.Errorf("invalid current version %s", cr.VersionDetails.Status.Current)
		}
		if !version.IsDesiredVersionValid(cr.VersionDetails.Desired) {
			return fmt.Errorf("invalid desired version %s", cr.VersionDetails.Desired)
		}
		jObj := cr.DeepCopy()
		if cr.VersionDetails.Status.State != openebsiov1alpha1.ReconcileInProgress {
			jObj.VersionDetails.Status.SetInProgressStatus()
			err = r.updateJivaVolume(jObj)
			if err != nil {
				return err
			}
		}
		// Update cr with the updated fields so that we don't get
		// resourceVersion changed error in next steps
		if err := r.getJivaVolume(cr); err != nil {
			return fmt.Errorf("%s, err: %v", updateErrMsg, err)
		}
		// As no other steps are required just change current version to
		// desired version
		path := strings.Split(jObj.VersionDetails.Status.Current, "-")[0]
		u := &upgradeParams{
			j:      jObj,
			client: r.Client,
		}
		// Get upgrade function for corresponding path, if path does not
		// exits then no upgrade is required and funcValue will be nil.
		funcValue := upgradeMap[path]
		if funcValue != nil {
			jObj, err = funcValue(u)
			if err != nil {
				return err
			}
		}
		cr = jObj.DeepCopy()
		jObj.VersionDetails.SetSuccessStatus()
		err = r.updateJivaVolume(jObj)
		if err != nil {
			return err
		}
		// Update cr with the updated fields so that we don't get
		// resourceVersion changed error in next steps
		if err := r.getJivaVolume(cr); err != nil {
			return fmt.Errorf("%s, err: %v", updateErrMsg, err)
		}
		return nil
	}
	return nil
}
