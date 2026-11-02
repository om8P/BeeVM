package apiclient

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/om8P/BeeVM/internal/apitypes"
)

// The fake runner has no warm pool: prewarm is 501 and the VM list is empty. That is enough to
// check the client's requests and decoding; the pool behaviour is tested in internal/api.
func TestPrewarmAndVMs(t *testing.T) {
	c, _, _ := setup(t)
	ctx := context.Background()
	if _, _, err := c.Deploy(ctx, "hello", bytes.NewReader(elf(5)), apitypes.FunctionConfig{}); err != nil {
		t.Fatal(err)
	}
	_, err := c.Prewarm(ctx, "hello", 2)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 501 {
		t.Errorf("want 501, got %v", err)
	}
	if _, err := c.Prewarm(ctx, "nope", 1); !IsNotFound(err) && !(errors.As(err, &ae) && ae.Status == 501) {
		t.Errorf("unknown function: %v", err)
	}
	vms, err := c.VMs(ctx)
	if err != nil || vms == nil || len(vms) != 0 {
		t.Errorf("VMs: %v, %v", vms, err)
	}
}
