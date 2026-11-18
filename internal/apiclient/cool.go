package apiclient

import (
	"context"
	"net/http"
	"strconv"

	"github.com/om8P/BeeVM/internal/apitypes"
)

// Cool throws away up to n of a function's warm VMs (n <= 0: all), so its next call is a cold
// start again. It returns how many VMs that affected.
func (c *Client) Cool(ctx context.Context, name string, n int) (int, error) {
	path := fnPath(name) + "/cool"
	if n > 0 {
		path += "?count=" + strconv.Itoa(n)
	}
	resp, err := c.do(ctx, http.MethodPost, path, nil, "")
	if err != nil {
		return 0, err
	}
	r, err := decodeBody[apitypes.CoolResult](resp)
	return r.Cooled, err
}

// CoolAll does Cool for every function of the caller.
func (c *Client) CoolAll(ctx context.Context) (int, error) {
	resp, err := c.do(ctx, http.MethodPost, "/api/v1/vms/cool", nil, "")
	if err != nil {
		return 0, err
	}
	r, err := decodeBody[apitypes.CoolResult](resp)
	return r.Cooled, err
}
