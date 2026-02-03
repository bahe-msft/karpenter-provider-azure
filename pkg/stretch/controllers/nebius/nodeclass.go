package nebius

import (
	"context"

	opcontroller "github.com/awslabs/operatorpkg/controller"
	"github.com/awslabs/operatorpkg/reasonable"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
)

type NodeClassController struct {
	kubeClient client.Client
}

var (
	_ opcontroller.Controller                                     = (*NodeClassController)(nil)
	_ reconcile.ObjectReconciler[*v1beta1.StretchNebiusNodeClass] = (*NodeClassController)(nil)
)

func NewNodeClassController(kubeClient client.Client) *NodeClassController {
	return &NodeClassController{
		kubeClient: kubeClient,
	}
}

func (c *NodeClassController) Register(ctx context.Context, mgr manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(mgr).
		Named("nebius.nodeclass").
		For(&v1beta1.StretchNebiusNodeClass{}).
		WithOptions(controller.Options{
			RateLimiter: reasonable.RateLimiter(),
			// TODO: Document why this magic number used. If we want to consistently use it accoss reconcilers, refactor to a reused const.
			// Comments thread discussing this: https://github.com/Azure/karpenter-provider-azure/pull/729#discussion_r2006629809
			MaxConcurrentReconciles: 10,
		}).
		Complete(reconcile.AsReconciler(mgr.GetClient(), c))
}

func (c *NodeClassController) Reconcile(
	ctx context.Context,
	object *v1beta1.StretchNebiusNodeClass,
) (reconcile.Result, error) {
	panic("unimplemented")
}
