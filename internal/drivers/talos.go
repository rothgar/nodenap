package drivers

import (
	"context"
	"fmt"

	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	clientconfig "github.com/siderolabs/talos/pkg/machinery/client/config"
)

// TalosShutdown shuts a Talos node down through the Talos API; node is used
// as both endpoint and target.
func TalosShutdown(ctx context.Context, talosconfig []byte, node string) error {
	cfg, err := clientconfig.FromBytes(talosconfig)
	if err != nil {
		return fmt.Errorf("parsing talosconfig: %w", err)
	}
	c, err := talosclient.New(ctx, talosclient.WithConfig(cfg), talosclient.WithEndpoints(node))
	if err != nil {
		return err
	}
	defer c.Close() //nolint:errcheck
	return c.Shutdown(talosclient.WithNodes(ctx, node))
}
