package drivers

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestOmniLiveLookup checks the real Omni client read-only (it only lists
// machines). Run with OMNI_LIVE_NODE=<node> OMNI_ENDPOINT=... OMNI_SERVICE_ACCOUNT_KEY=...
func TestOmniLiveLookup(t *testing.T) {
	node := os.Getenv("OMNI_LIVE_NODE")
	if node == "" {
		t.Skip("set OMNI_LIVE_NODE, OMNI_ENDPOINT and OMNI_SERVICE_ACCOUNT_KEY to run against a real Omni")
	}
	api, err := NewOmniAPI(os.Getenv("OMNI_ENDPOINT"), os.Getenv("OMNI_SERVICE_ACCOUNT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close() //nolint:errcheck
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := api.MachineIDForNode(ctx, node)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("node %s → Omni machine %s", node, id)
}
