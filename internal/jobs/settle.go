package jobs

import (
	"context"
	"errors"
)

// Settle ends a job from its worker's error. A cancel outranks the error:
// a cancelled worker returns context.Canceled, which is not a failure.
func (m *Manager) Settle(ctx context.Context, err error, cancelMsg, doneMsg string) error {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		m.Complete(cancelMsg)
		return nil
	}
	if err != nil {
		m.Fail(err.Error())
		return err
	}
	m.Complete(doneMsg)
	return nil
}

func (m *Manager) Finish(err error, cancelled bool, cancelMsg, doneMsg string) {
	switch {
	case err != nil:
		m.Fail(err.Error())
	case cancelled:
		m.Complete(cancelMsg)
	default:
		m.Complete(doneMsg)
	}
}
