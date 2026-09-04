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

var abCmd = &cobra.Command{
	Use:   "ab [port <n> | node <host> | off]",
	Short: "Run a paired A/B experiment on the UDP leg and read the result",
	Long: `Compare two hy2 outbounds that differ in exactly one variable, sampled in the
same seconds, and report which is better with a p-value.

Why paired: the UDP leg's failure is intermittent at a scale that swamps
sequential trials — 73% of one afternoon and 0% of another day, on the same node
over the same route. An arm measured Tuesday and an arm measured Wednesday
cannot be compared, and that is how every previous verdict on this tunnel
(port hopping, obfs) was reached. Sampling both arms in the same seconds makes
the episode common-mode; what survives the subtraction is the variable.

  mole ab port <n>     control = the live UDP port, arm = <n>. Needs no server
                       change: the boot script already NATs the whole hop range
                       onto the listen port, so any port in it is answered.
  mole ab node <host>  control = the live node, arm = another host on the same
                       port. For comparing providers, regions, or two server
                       implementations.
  mole ab              show the running experiment and its verdict.
  mole ab off          end it.

Arms are probe-only: they are materialized as outbounds so the Clash API can
dial them, and deliberately kept out of the traffic selector, so no real request
is ever routed through an experimental port.

Declaring or ending an experiment changes the sing-box config, so it needs
"mole down && mole up" to take. Reading one does not.`,
	Args: cobra.MaximumNArgs(2),
	RunE: runAB,
}

func init() { rootCmd.AddCommand(abCmd) }

// abHopLow/abHopHigh bound the UDP range the server's porthop unit redirects
// onto the listen port. Kept in step with deploy_vultr.go's hopPortLow/High:
// a port outside it reaches nothing and the arm would measure a closed door.
const (
	abHopLow  = 20000
	abHopHigh = 50000
)

func runAB(_ *cobra.Command, args []string) error {
	if len(args) == 0 {
		return printAB()
	}
	switch strings.ToLower(args[0]) {
	case "off", "stop", "clear":
		if err := utils.ClearExperiment(); err != nil {
			return fmt.Errorf("end experiment: %w", err)
		}
		fmt.Println("✔ Experiment ended. Run \"mole down && mole up\" to drop the probe-only arms from the config.")
		return nil
	case "port":
		if len(args) < 2 {
			return fmt.Errorf("which port? e.g. \"mole ab port 20443\"")
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("expected a port number, got %q", args[1])
		}
		srv, err := ActiveServer()
		if err != nil {
			return err
		}
		if n < abHopLow || n > abHopHigh {
			// Not fatal: someone may have opened a port by hand. But an arm that
			// silently measures a closed port would read as "UDP is broken here",
			// which is the exact conclusion this whole experiment exists to test.
			fmt.Printf("⚠️  %d is outside the range the server NATs onto its listen port (%d-%d).\n", n, abHopLow, abHopHigh)
			fmt.Println("   Unless you opened it by hand, this arm will measure a closed port, not a throttled one.")
		}
		return declareAB(&utils.Experiment{
			Name: fmt.Sprintf("udp-port-%d", n),
			Note: fmt.Sprintf("does moving off UDP/%d lift the penalty?", srv.Port),
			Arms: []utils.ExperimentArm{
				{Name: fmt.Sprintf("p%d", srv.Port), Note: "control — the live port"},
				{Name: fmt.Sprintf("p%d", n), Note: "same node, same everything, different UDP port", Port: n},
			},
		})
	case "node":
		if len(args) < 2 {
			return fmt.Errorf("which host? e.g. \"mole ab node 1.2.3.4\"")
		}
		srv, err := ActiveServer()
		if err != nil {
			return err
		}
		if args[1] == srv.IP {
			return fmt.Errorf("%s is the live node — an arm has to differ from the control in something", args[1])
		}
		return declareAB(&utils.Experiment{
			Name: "node-" + args[1],
			Note: "same protocol and credentials, different box",
			Arms: []utils.ExperimentArm{
				{Name: "live", Note: "control — " + srv.IP},
				{Name: "alt", Note: args[1], Server: args[1]},
			},
		})
	}
	return fmt.Errorf("expected \"port <n>\", \"node <host>\" or \"off\", got %q", args[0])
}

func declareAB(e *utils.Experiment) error {
	if old := utils.ReadExperiment(); old != nil && old.Name != e.Name {
		// Only one at a time, deliberately: two experiments running together each
		// add probe traffic to the other's measurement, and the whole point is to
		// hold everything but one variable still.
		return fmt.Errorf("experiment %q is already running (%d arms) — end it with \"mole ab off\" first", old.Name, len(old.Arms))
	}
	e.Started = time.Now().Format(time.RFC3339)
	if err := utils.WriteExperiment(e); err != nil {
		return err
	}
	fmt.Printf("✔ Experiment %q declared:\n", e.Name)
	for _, a := range e.Arms {
		fmt.Printf("     %-10s %s\n", a.Name, a.Note)
	}
	fmt.Println("   Run \"mole down && mole up\" to materialize the arms, then leave it running.")
	fmt.Printf("   Read it with \"mole ab\" — it needs %d samples per arm before it will call anything.\n", core.ExperimentMinPaired)
	return nil
}

func printAB() error {
	e := utils.ReadExperiment()
	if e == nil {
		fmt.Println("No experiment running.")
		fmt.Println("  mole ab port <n>     compare the live UDP port against another")
		fmt.Println("  mole ab node <host>  compare this node against another box")
		return nil
	}
	fmt.Printf("Experiment: %s", e.Name)
	if e.Started != "" {
		if t, err := time.Parse(time.RFC3339, e.Started); err == nil {
			fmt.Printf(" — running %s", humanize(time.Since(t)))
		}
	}
	fmt.Println()
	if e.Note != "" {
		fmt.Printf("            %s\n", e.Note)
	}
	for i := range e.Arms {
		a := &e.Arms[i]
		n := len(a.Samples)
		if n == 0 {
			fmt.Printf("  %-10s no samples yet — did you \"mole down && mole up\" after declaring it?\n", a.Name)
			continue
		}
		fmt.Printf("  %-10s %3d/%-3d bad (%.0f%%)   %s\n", a.Name, a.ArmBad(), n, 100*float64(a.ArmBad())/float64(n), a.Note)
	}
	if len(e.Arms) == 2 {
		fmt.Printf("  → %s\n", core.ExperimentVerdict(&e.Arms[0], &e.Arms[1]))
	}
	return nil
}
