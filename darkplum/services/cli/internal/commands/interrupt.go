package commands

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// watchInterrupts turns Ctrl-C into the two-stage stop a Terraform run needs. The
// first interrupt calls cancel: a run in flight is asked, once, to stop cleanly
// (finishing the resource it is on and releasing the state lock). The second
// closes force, which kills it, and prints how to clear a lock that may be left;
// if the process is still running ten seconds after that (it was not in
// Terraform), it exits. stop ends the watching.
func watchInterrupts(out io.Writer, cancel func()) (force <-chan struct{}, stop func()) {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	f := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	go func() {
		n := 0
		for {
			select {
			case <-sigs:
				n++
				if n == 1 {
					fmt.Fprintln(out, "\n"+sWarn("! interrupted: asking Terraform to stop cleanly. It finishes the resource it is working on and releases the state lock, which can take minutes. Ctrl-C again to kill it."))
					cancel()
					continue
				}
				once.Do(func() {
					fmt.Fprintln(out, "\n"+sFail("✗ killing Terraform now.")+
						" It may have left the state locked or a resource half-made. The next run reports a lock with its id;\n  `terraform force-unlock <id>` clears it, but only when nothing else is running. Re-run to continue.")
					close(f)
					go func() { time.Sleep(10 * time.Second); os.Exit(130) }()
				})
			case <-done:
				return
			}
		}
	}()
	return f, func() { signal.Stop(sigs); close(done) }
}
