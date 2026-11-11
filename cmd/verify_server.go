package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

func init() {
	verify := &cobra.Command{Use: "verify [name]", Short: "Verify an existing node without creating or repairing resources", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		servers, err := loadServers()
		if err != nil {
			return err
		}
		idx := -1
		if len(args) > 0 {
			idx = indexByName(servers, args[0])
		} else {
			for i, s := range servers {
				if s.Active {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			return fmt.Errorf("server not found")
		}
		budget, _ := cmd.Flags().GetDuration("timeout")
		if budget <= 0 {
			return fmt.Errorf("--timeout must be positive")
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return verifyRecordedNode(ctx, servers[idx], budget)
	}}
	verify.Flags().Duration("timeout", nodeVerifyBudget, "maximum address wait and protocol verification time")
	serverCmd.AddCommand(verify)
}

func verifyRecordedNode(parent context.Context, srv Server, budget time.Duration) error {
	if srv.Stopped {
		return fmt.Errorf("server is stopped; verification does not start it")
	}
	// Probes to a node that is not carrying the tunnel route through utun and
	// hairpin — the direct-UDP probe in particular can never pass that way.
	// The active server is exempt: its address is the one the tunnel excludes.
	if st, err := utils.ReadState(); err == nil && st != nil && st.PID > 0 && st.Server != srv.Name && processAlive(st.PID) {
		return fmt.Errorf("the tunnel is running on %s; probes to %s would hairpin through it — run mole down first", st.Server, srv.Name)
	}
	if srv.DeployID == "" {
		id, err := randomHexString(16)
		if err != nil {
			return err
		}
		srv.DeployID = id
	}
	session, err := newDeploySession(srv, "")
	if err != nil {
		return err
	}
	defer session.log.Close()
	session.record.InstanceID = srv.InstanceID
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	if !srv.HasAddress() {
		if srv.InstanceID == "" {
			return fmt.Errorf("server has neither an address nor provider instance ID")
		}
		ip, ipv6, err := waitForIP(ctx, srv.InstanceID)
		if err != nil {
			_ = session.stage("provisioning", err)
			return err
		}
		srv.IP, srv.IPv6 = ip, ipv6
	}
	if err = session.stage("verifying", nil); err != nil {
		return err
	}
	result, verifyErr := verifyNode(ctx, srv, session.writer())
	session.record.Verification = &result
	storeErr := changeServer(srv.InstanceID, srv.Name, func(current *Server) {
		current.IP = srv.IP
		current.IPv6 = srv.IPv6
		current.DeployID = srv.DeployID
		current.Verification = &result
	})
	recordErr := session.stage(result.Status, verifyErr)
	fmt.Printf("Verification receipt: %s\n", filepath.Join(session.dir, "receipt.json"))
	return errors.Join(verifyErr, storeErr, recordErr)
}
