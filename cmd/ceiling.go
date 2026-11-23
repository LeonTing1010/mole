package cmd

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/LeonTing1010/mole/core"
	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var ceilingCmd = &cobra.Command{
	Use:   "ceiling [mbps|auto|tune]",
	Short: "Set the Hysteria2 bandwidth mode",
	Long: `Pin the hysteria2 Brutal ceiling to one of the ladder's rungs, or hand control
back to the clock with "auto". "tune" uses the current ceiling as the starting
point for a one-time download comparison on each startup in Hysteria2 mode.

The switch is a single Clash-API call against a pre-built outbound, so the TUN,
DNS and in-flight connections are untouched — no reconnect, no dropped packets.
Only ceilings that were materialized as rungs at "mole up" time can be selected;
run with no argument to list them.

Changing the ladder itself (the servers.json up_mbps/down_mbps/peak_* values)
still needs "mole down && mole up", because sing-box bakes an outbound's declared
bandwidth in at config load and no API can mutate it.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runCeiling,
}

func init() { rootCmd.AddCommand(ceilingCmd) }

func runCeiling(_ *cobra.Command, args []string) error {
	srv, err := ActiveServer()
	if err != nil {
		return err
	}
	sched := srv.Schedule()
	rungs := sched.Rungs()
	if len(rungs) < 2 {
		return fmt.Errorf("server %q has no bandwidth ladder (needs a Brutal ceiling: set up_mbps/down_mbps in %s)", srv.Name, utils.ServersPath())
	}

	if len(args) == 0 {
		printCeiling(srv, sched, rungs)
		if srv.RealityDownMbps > 0 {
			fmt.Printf("REALITY TCP Brutal: %d Mbps (independent of the hy2 ladder)\n", srv.RealityDownMbps)
		}
		return nil
	}

	// Resolve the request to a target rung before touching anything, so a typo
	// leaves the pin and the live selector exactly as they were.
	arg := strings.ToLower(strings.TrimSpace(args[0]))
	if arg == "tune" {
		seed := currentRung(srv)
		if err := utils.WriteCeilingSetting(utils.CeilingSetting{Down: seed, Tune: true}); err != nil {
			return fmt.Errorf("enable startup ceiling tuning: %w", err)
		}
		fmt.Printf("Startup Hysteria2 tuning enabled (seed: %d Mbps). Applies on the next mole up.\n", seed)
		if srv.StandbyURI() != "" && utils.ReadLeg() != utils.LegHy2 {
			fmt.Println("On dual-transport nodes, select \"mole leg hy2\" to tune Hysteria2.")
		}
		return nil
	}
	var target int
	if arg == "auto" {
		_, _, target = sched.Profile(time.Now())
	} else {
		n, err := strconv.Atoi(arg)
		if err != nil {
			return fmt.Errorf("expected an Mbps number, \"auto\" or \"tune\", got %q", args[0])
		}
		if !sched.HasRung(n) {
			return fmt.Errorf("no rung at %d Mbps — this config has %s", n, formatRungs(rungs))
		}
		target = n
	}

	// Persist the intent first. If the Clash call below fails (daemon down), the
	// pin still applies the moment the supervisor next ticks, so the two paths
	// can't disagree about what the user asked for.
	if arg == "auto" {
		if err := utils.ClearCeiling(); err != nil {
			return fmt.Errorf("clear ceiling pin: %w", err)
		}
	} else if err := utils.WriteCeiling(target); err != nil {
		return fmt.Errorf("write ceiling pin: %w", err)
	}

	pid, perr := utils.ReadPID()
	if perr != nil || !utils.IsRunning(pid) {
		fmt.Printf("Ceiling pinned to %d Mbps — will apply when mole starts.\n", target)
		return nil
	}

	// Pre-warm before switching for the same reason the supervisor does: the rung
	// holds no QUIC session until something dials it, and paying that handshake
	// here keeps the first post-switch connection from eating it.
	member := core.BandwidthRungTag(target)
	clash := core.NewClashClient("127.0.0.1:9090")
	// Don't yank traffic off REALITY. It is the primary, and the ceiling is a
	// property of the hy2 ladder only; selecting a rung here would move live
	// traffic across transports under the name of a ceiling change — which the
	// scheduler would undo on its next tick, so the only lasting effect would
	// be the churn in between. The pin is saved either way and takes effect
	// whenever the ladder carries traffic: a fallback, or `mole leg hy2`.
	if now, err := clash.Selection(core.ProxySelectorTag); err == nil && now == core.RealityLegTag {
		tcpRate := target
		if srv.RealityDownMbps > 0 {
			tcpRate = srv.RealityDownMbps
		}
		fmt.Printf("✔ HY2 ceiling → %d Mbps. Traffic stays on REALITY; the node paces TCP toward this client at %d Mbps.\n", target, tcpRate)
		return nil
	}
	if _, err := clash.TestDelay(member, "https://www.gstatic.com/generate_204", 5000); err != nil {
		fmt.Printf("   (pre-warm of %s failed: %v — switching anyway)\n", member, err)
	}
	if err := clash.SelectProxy(core.ProxySelectorTag, member); err != nil {
		return fmt.Errorf("select %s: %w (pin is saved; the supervisor will retry within a minute)", member, err)
	}

	if arg == "auto" {
		fmt.Printf("✔ Ceiling → auto (clock): now %d Mbps via %s\n", target, member)
	} else {
		fmt.Printf("✔ Ceiling → %d Mbps via %s (pinned)\n", target, member)
	}
	fmt.Println("   No reconnect — TUN and existing connections untouched.")
	return nil
}

func printCeiling(srv *Server, sched core.BandwidthSchedule, rungs []int) {
	name, up, down := sched.Profile(time.Now())
	setting := utils.ReadCeilingSetting()
	if setting.Tune {
		baseline := sched.StartupTuneBaseline(setting.Down, utils.ReadTunedCeiling(srv.Name, setting.Down), time.Now())
		fmt.Printf("Ceiling: startup tuning, seed %d Mbps; starting rung %d Mbps\n", setting.Down, baseline)
	} else if pin := setting.Down; pin > 0 {
		state := "pinned"
		if !sched.HasRung(pin) {
			state = "pinned but NOT in this config — the clock is in control"
		}
		fmt.Printf("Ceiling: %d Mbps (%s)\n", pin, state)
	} else {
		fmt.Printf("Ceiling: %d Mbps ↓ / %d Mbps ↑ — auto, %s window\n", down, up, name)
	}
	fmt.Printf("Rungs:   %s\n", formatRungs(rungs))
	fmt.Println("         mole ceiling <mbps>   pin one")
	fmt.Println("         mole ceiling tune     benchmark at each startup")
	fmt.Println("         mole ceiling auto     back to the clock")
}

func formatRungs(rungs []int) string {
	parts := make([]string, len(rungs))
	for i, n := range rungs {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ") + " Mbps"
}
