package budget

import (
	"context"
	"errors"
	"fmt"

	"github.com/bharathvbcr/Manvi/manvi/internal/filelock"
)

// lockFile takes the ledger's cross-process lock on path, waiting until ctx
// ends. The lock itself is internal/filelock's, shared with the workflow
// catalog; what is the ledger's is the refusal: a cap that cannot be shared
// safely must stop the provider from starting, not let concurrent sessions
// overspend it.
func lockFile(ctx context.Context, path string) (func(), error) {
	unlock, err := filelock.Lock(ctx, path)
	switch {
	case err == nil:
		return unlock, nil
	case errors.Is(err, filelock.ErrUnsupported):
		return nil, errors.New("budget: no cross-process file lock on this platform; the spend cap cannot be enforced")
	case ctx.Err() != nil && errors.Is(err, ctx.Err()):
		return nil, err
	default:
		return nil, fmt.Errorf("budget: %w", err)
	}
}
