package reconciler

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/go-logr/logr"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	anywherev1 "github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	"github.com/aws/eks-anywhere/pkg/clients/kubernetes"
	c "github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/config"
	"github.com/aws/eks-anywhere/pkg/controller"
	"github.com/aws/eks-anywhere/pkg/controller/clientutil"
	"github.com/aws/eks-anywhere/pkg/controller/serverside"
	"github.com/aws/eks-anywhere/pkg/providers/vsphere"
)

// CNIReconciler is an interface for reconciling CNI in the VSphere cluster reconciler.
type CNIReconciler interface {
	Reconcile(ctx context.Context, logger logr.Logger, client client.Client, spec *c.Spec) (controller.Result, error)
}

// RemoteClientRegistry is an interface that defines methods for remote clients.
type RemoteClientRegistry interface {
	GetClient(ctx context.Context, cluster client.ObjectKey) (client.Client, error)
}

type Reconciler struct {
	client               client.Client
	validator            *vsphere.Validator
	defaulter            *vsphere.Defaulter
	cniReconciler        CNIReconciler
	remoteClientRegistry RemoteClientRegistry
	*serverside.ObjectApplier
}

// New defines a new VSphere reconciler.
func New(client client.Client, validator *vsphere.Validator, defaulter *vsphere.Defaulter, cniReconciler CNIReconciler, remoteClientRegistry RemoteClientRegistry) *Reconciler {
	return &Reconciler{
		client:               client,
		validator:            validator,
		defaulter:            defaulter,
		cniReconciler:        cniReconciler,
		remoteClientRegistry: remoteClientRegistry,
		ObjectApplier:        serverside.NewObjectApplier(client),
	}
}

func VsphereCredentials(ctx context.Context, cli client.Client) (*apiv1.Secret, error) {
	secret := &apiv1.Secret{}
	secretKey := client.ObjectKey{
		Namespace: "eksa-system",
		Name:      vsphere.CredentialsObjectName,
	}
	if err := cli.Get(ctx, secretKey, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func SetupEnvVars(ctx context.Context, vsphereDatacenter *anywherev1.VSphereDatacenterConfig, cli client.Client) error {
	secret, err := VsphereCredentials(ctx, cli)
	if err != nil {
		return fmt.Errorf("failed getting vsphere credentials secret: %v", err)
	}

	vsphereUsername := secret.Data["username"]
	vspherePassword := secret.Data["password"]

	if err := os.Setenv(config.EksavSphereUsernameKey, string(vsphereUsername)); err != nil {
		return fmt.Errorf("failed setting env %s: %v", config.EksavSphereUsernameKey, err)
	}

	if err := os.Setenv(config.EksavSpherePasswordKey, string(vspherePassword)); err != nil {
		return fmt.Errorf("failed setting env %s: %v", config.EksavSpherePasswordKey, err)
	}

	if err := vsphere.SetupEnvVars(vsphereDatacenter); err != nil {
		return fmt.Errorf("failed setting env vars: %v", err)
	}

	return nil
}

func (r *Reconciler) Reconcile(ctx context.Context, log logr.Logger, cluster *anywherev1.Cluster) (controller.Result, error) {
	log = log.WithValues("provider", "vsphere")
	clusterSpec, err := c.BuildSpec(ctx, clientutil.NewKubeClient(r.client), cluster)
	if err != nil {
		return controller.Result{}, err
	}

	return controller.NewPhaseRunner().Register(
		r.ValidateDatacenterConfig,
		r.ValidateMachineConfigs,
		r.ReconcileControlPlane,
		r.ReconcileCNI,
		r.ReconcileWorkers,
	).Run(ctx, log, clusterSpec)
}

// ValidateDatacenterConfig updates the cluster status if the VSphereDatacenter status indicates that the spec is invalid.
func (r *Reconciler) ValidateDatacenterConfig(ctx context.Context, log logr.Logger, clusterSpec *c.Spec) (controller.Result, error) {
	log = log.WithValues("phase", "validateDatacenterConfig")
	dataCenterConfig := clusterSpec.VSphereDatacenter

	if !dataCenterConfig.Status.SpecValid {
		var failureMessage string
		if dataCenterConfig.Status.FailureMessage != nil {
			failureMessage = *dataCenterConfig.Status.FailureMessage
		}

		log.Error(errors.New(failureMessage), "Invalid VSphereDatacenterConfig", "datacenterConfig", klog.KObj(dataCenterConfig))
		clusterSpec.Cluster.Status.FailureMessage = &failureMessage
		return controller.Result{}, nil
	}
	return controller.Result{}, nil
}

// ValidateMachineConfigs performs additional, context-aware validations on the machine configs.
func (r *Reconciler) ValidateMachineConfigs(ctx context.Context, log logr.Logger, clusterSpec *c.Spec) (controller.Result, error) {
	log = log.WithValues("phase", "validateMachineConfigs")
	datacenterConfig := clusterSpec.VSphereDatacenter

	// Set up env vars for executing Govc cmd
	if err := SetupEnvVars(ctx, datacenterConfig, r.client); err != nil {
		log.Error(err, "Failed to set up env vars for Govc")
		return controller.Result{}, err
	}

	vsphereClusterSpec := vsphere.NewSpec(clusterSpec)

	if err := r.validator.ValidateClusterMachineConfigs(ctx, vsphereClusterSpec); err != nil {
		log.Error(err, "Invalid VSphereMachineConfig")
		failureMessage := err.Error()
		clusterSpec.Cluster.Status.FailureMessage = &failureMessage
		return controller.Result{}, err
	}
	return controller.Result{}, nil
}

// ReconcileControlPlane applies the control plane CAPI objects to the cluster.
func (r *Reconciler) ReconcileControlPlane(ctx context.Context, log logr.Logger, clusterSpec *c.Spec) (controller.Result, error) {
	log = log.WithValues("phase", "reconcileControlPlane")
	log.Info("Applying control plane CAPI objects")
	// TODO: implement CP reconciliation phase
	return controller.Result{}, nil
}

// ReconcileCNI takes the Cilium CNI in a cluster to the desired state defined in a cluster spec.
func (r *Reconciler) ReconcileCNI(ctx context.Context, log logr.Logger, clusterSpec *c.Spec) (controller.Result, error) {
	log = log.WithValues("phase", "reconcileCNI")
	client, err := r.remoteClientRegistry.GetClient(ctx, controller.CapiClusterObjectKey(clusterSpec.Cluster))
	if err != nil {
		return controller.Result{}, err
	}

	return r.cniReconciler.Reconcile(ctx, log, client, clusterSpec)
}

// ReconcileWorkers applies the worker CAPI objects to the cluster.
func (r *Reconciler) ReconcileWorkers(ctx context.Context, log logr.Logger, clusterSpec *c.Spec) (controller.Result, error) {
	log = log.WithValues("phase", "reconcileWorkers")
	log.Info("Applying worker CAPI objects")
	return r.Apply(ctx, func() ([]kubernetes.Object, error) {
		w, err := vsphere.WorkersSpec(ctx, log, clientutil.NewKubeClient(r.client), clusterSpec)
		if err != nil {
			return nil, err
		}
		return w.WorkerObjects(), nil
	})
}
