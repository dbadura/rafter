package controllers

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"github.com/kyma-project/rafter/internal/finalizer"
	"github.com/kyma-project/rafter/internal/handler/bucket"
	"github.com/kyma-project/rafter/internal/store"
	assetstorev1beta1 "github.com/kyma-project/rafter/pkg/apis/rafter/v1beta1"
	"github.com/pkg/errors"
	apiErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

const deleteClusterBucketFinalizerName = "deleteclusterbucket.finalizers.rafter.kyma-project.io"

// ClusterBucketReconciler reconciles a ClusterBucket object
type ClusterBucketReconciler struct {
	client.Client
	Log logr.Logger

	cacheSynchronizer       func(stop <-chan struct{}) bool
	recorder                record.EventRecorder
	relistInterval          time.Duration
	finalizer               finalizer.Finalizer
	store                   store.Store
	externalEndpoint        string
	maxConcurrentReconciles int
}

type ClusterBucketConfig struct {
	MaxConcurrentReconciles int           `envconfig:"default=1"`
	RelistInterval          time.Duration `envconfig:"default=30s"`
	ExternalEndpoint        string        `envconfig:"-"`
}

func NewClusterBucket(config ClusterBucketConfig, log logr.Logger, di *Container) *ClusterBucketReconciler {
	deleteFinalizer := finalizer.New(deleteClusterBucketFinalizerName)

	return &ClusterBucketReconciler{
		Client:                  di.Manager.GetClient(),
		cacheSynchronizer:       di.Manager.GetCache().WaitForCacheSync,
		Log:                     log,
		recorder:                di.Manager.GetEventRecorderFor("clusterbucket-controller"),
		relistInterval:          config.RelistInterval,
		store:                   di.Store,
		finalizer:               deleteFinalizer,
		externalEndpoint:        config.ExternalEndpoint,
		maxConcurrentReconciles: config.MaxConcurrentReconciles,
	}
}

// Reconcile reads that state of the cluster for a ClusterBucket object and makes changes based on the state read
// +kubebuilder:rbac:groups=rafter.kyma-project.io,resources=clusterbuckets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rafter.kyma-project.io,resources=clusterbuckets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ClusterBucketReconciler) Reconcile(request ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithCancel(context.TODO())
	defer cancel()

	if err := r.appendFinalizer(ctx, request.NamespacedName); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "while appending finalizer")
	}

	instance := &assetstorev1beta1.ClusterBucket{}
	err := r.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if apiErrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return ctrl.Result{}, err
	}

	bucketLogger := r.Log.WithValues("kind", instance.GetObjectKind().GroupVersionKind().Kind, "name", instance.GetName())
	commonHandler := bucket.New(bucketLogger, r.recorder, r.store, r.externalEndpoint, r.relistInterval)
	commonStatus, err := commonHandler.Do(ctx, time.Now(), instance, instance.Spec.CommonBucketSpec, instance.Status.CommonBucketStatus)
	if updateErr := r.updateStatus(ctx, request.NamespacedName, commonStatus); updateErr != nil {
		finalErr := updateErr
		if err != nil {
			finalErr = errors.Wrapf(err, "along with update error %s", updateErr.Error())
		}
		return ctrl.Result{}, finalErr
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.removeFinalizer(ctx, request.NamespacedName); err != nil {
		return ctrl.Result{}, errors.Wrap(err, "while removing finalizer")
	}

	return ctrl.Result{
		RequeueAfter: r.relistInterval,
	}, nil
}

func (r *ClusterBucketReconciler) appendFinalizer(ctx context.Context, namespacedName types.NamespacedName) error {
	updateFnc := func(instance *assetstorev1beta1.ClusterBucket) error {
		if !instance.DeletionTimestamp.IsZero() || r.finalizer.IsDefinedIn(instance) {
			return nil
		}

		copy := instance.DeepCopy()
		r.finalizer.AddTo(copy)
		return r.Update(ctx, copy)
	}

	return r.update(ctx, namespacedName, updateFnc)
}

func (r *ClusterBucketReconciler) removeFinalizer(ctx context.Context, namespacedName types.NamespacedName) error {
	updateFnc := func(instance *assetstorev1beta1.ClusterBucket) error {
		if instance.DeletionTimestamp.IsZero() {
			return nil
		}

		copy := instance.DeepCopy()
		r.finalizer.DeleteFrom(copy)

		return r.Update(ctx, copy)
	}

	return r.update(ctx, namespacedName, updateFnc)
}

func (r *ClusterBucketReconciler) updateStatus(ctx context.Context, namespacedName types.NamespacedName, commonStatus *assetstorev1beta1.CommonBucketStatus) error {
	updateFnc := func(instance *assetstorev1beta1.ClusterBucket) error {
		if r.isStatusUnchanged(instance, commonStatus) {
			return nil
		}

		copy := instance.DeepCopy()
		copy.Status.CommonBucketStatus = *commonStatus

		return r.Status().Update(ctx, copy)
	}

	return r.update(ctx, namespacedName, updateFnc)
}

func (r *ClusterBucketReconciler) isStatusUnchanged(instance *assetstorev1beta1.ClusterBucket, newStatus *assetstorev1beta1.CommonBucketStatus) bool {
	currentStatus := instance.Status.CommonBucketStatus

	return newStatus == nil ||
		currentStatus.ObservedGeneration == newStatus.ObservedGeneration &&
			currentStatus.Phase == newStatus.Phase &&
			currentStatus.Reason == newStatus.Reason
}

func (r *ClusterBucketReconciler) update(ctx context.Context, namespacedName types.NamespacedName, updateFnc func(instance *assetstorev1beta1.ClusterBucket) error) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		instance := &assetstorev1beta1.ClusterBucket{}
		err := r.Get(ctx, namespacedName, instance)
		if err != nil {
			if apiErrors.IsNotFound(err) {
				return nil
			}
			// Error reading the object - requeue the request.
			return err
		}

		err = updateFnc(instance)
		if err != nil && apiErrors.IsConflict(err) {
			r.cacheSynchronizer(ctx.Done())
		}

		return err
	})

	return err
}

func (r *ClusterBucketReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&assetstorev1beta1.ClusterBucket{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.maxConcurrentReconciles,
		}).
		Complete(r)
}
