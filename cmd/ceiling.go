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
	Use:   "ceiling [mbps|auto]",
	Short: "Pin the Brutal down-ceiling without reconnecting",
	Long: `Pin the hysteria2 Brutal ceiling to one of the ladder's rungs, or hand control
back to the clock with "auto".

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
		printCeiling(sched, rungs)
		return nil
	}

	// Resolve the request to a target rung before touching anything, so a typo
	// leaves the pin and the live selector exactly as they were.
	arg := strings.ToLower(strings.TrimSpace(args[0]))
	var target int
	if arg == "auto" {
		_, _, target = sched.Profile(time.Now())
	} else {
		n, err := strconv.Atoi(arg)
		if err != nil {
			return fmt.Errorf("expected an Mbps number or \"auto\", got %q", args[0])
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

func printCeiling(sched core.BandwidthSchedule, rungs []int) {
	name, up, down := sched.Profile(time.Now())
	if pin := utils.ReadCeiling(); pin > 0 {
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
	fmt.Println("         mole ceiling auto     back to the clock")
}

func formatRungs(rungs []int) string {
	parts := make([]string, len(rungs))
	for i, n := range rungs {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ") + " Mbps"
}
