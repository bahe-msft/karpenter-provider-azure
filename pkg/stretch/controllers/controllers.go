package controllers

import (
	"context"

	"github.com/awslabs/operatorpkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/karpenter/pkg/events"

	"github.com/Azure/karpenter-provider-azure/pkg/stretch/controllers/nebius"
	"github.com/Azure/karpenter-provider-azure/pkg/stretch/controllers/nodes"
)

func NewControllers(
	ctx context.Context,
	kubeClient client.Client,
	recorder events.Recorder,
) []controller.Controller {
	return []controller.Controller{
		// TODO: implement node class hash logic for drift detection/reconciliation
		nebius.NewNodeClassStatusController(kubeClient),
		nebius.NewNodeClassTerminationController(kubeClient, recorder),

		nodes.NewProviderIDController(kubeClient),
	}
}
