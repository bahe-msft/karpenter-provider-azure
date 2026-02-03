package controllers

import (
	"context"

	"github.com/awslabs/operatorpkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Azure/karpenter-provider-azure/pkg/stretch/controllers/nebius"
)

func NewControllers(
	ctx context.Context,
	kubeClient client.Client,
) []controller.Controller {
	return []controller.Controller{
		nebius.NewNodeClassController(kubeClient),
	}
}
