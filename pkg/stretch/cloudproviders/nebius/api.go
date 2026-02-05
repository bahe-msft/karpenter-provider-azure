package nebius

import (
	"context"

	"github.com/nebius/gosdk/operations"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func isNotFound(err error) bool {
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.NotFound
	}
	return false
}

func ignoreNotFound(err error) error {
	if isNotFound(err) {
		return nil
	}
	return err
}

func isAlreadyExists(err error) bool {
	if s, ok := status.FromError(err); ok {
		return s.Code() == codes.AlreadyExists
	}
	return false
}

type AsyncOperation interface {
	Wait(ctx context.Context) error
}

type completedOperation struct{}

func newCompletedOperation() AsyncOperation {
	return &completedOperation{}
}

func (o *completedOperation) Wait(ctx context.Context) error {
	return nil
}

type nebiusAsyncOperation struct {
	op operations.Operation
}

func newAsyncOperationFromNebius(op operations.Operation) AsyncOperation {
	return &nebiusAsyncOperation{op: op}
}

func (o *nebiusAsyncOperation) Wait(ctx context.Context) error {
	_, err := o.op.Wait(ctx)
	return err
}
