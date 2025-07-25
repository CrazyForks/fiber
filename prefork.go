package fiber

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp/reuseport"

	"github.com/gofiber/fiber/v3/log"
)

const (
	envPreforkChildKey = "FIBER_PREFORK_CHILD"
	envPreforkChildVal = "1"
	sleepDuration      = 100 * time.Millisecond
)

var (
	testPreforkMaster = false
	testOnPrefork     = false
)

// IsChild determines if the current process is a child of Prefork
func IsChild() bool {
	return os.Getenv(envPreforkChildKey) == envPreforkChildVal
}

// prefork manages child processes to make use of the OS REUSEPORT or REUSEADDR feature
func (app *App) prefork(addr string, tlsConfig *tls.Config, cfg ListenConfig) error {
	var ln net.Listener
	var err error

	// 👶 child process 👶
	if IsChild() {
		// use 1 cpu core per child process
		runtime.GOMAXPROCS(1)
		// Linux will use SO_REUSEPORT and Windows falls back to SO_REUSEADDR
		// Only tcp4 or tcp6 is supported when preforking, both are not supported
		if ln, err = reuseport.Listen(cfg.ListenerNetwork, addr); err != nil {
			if !cfg.DisableStartupMessage {
				time.Sleep(sleepDuration) // avoid colliding with startup message
			}
			return fmt.Errorf("prefork: %w", err)
		}
		// wrap a tls config around the listener if provided
		if tlsConfig != nil {
			ln = tls.NewListener(ln, tlsConfig)
		}

		// kill current child proc when master exits
		go watchMaster()

		// prepare the server for the start
		app.startupProcess()

		if cfg.ListenerAddrFunc != nil {
			cfg.ListenerAddrFunc(ln.Addr())
		}

		// listen for incoming connections
		return app.server.Serve(ln)
	}

	// 👮 master process 👮
	type child struct {
		err error
		pid int
	}
	// create variables
	maxProcs := runtime.GOMAXPROCS(0)
	children := make(map[int]*exec.Cmd)
	channel := make(chan child, maxProcs)

	// kill child procs when master exits
	defer func() {
		for _, proc := range children {
			if err = proc.Process.Kill(); err != nil {
				if !errors.Is(err, os.ErrProcessDone) {
					log.Errorf("prefork: failed to kill child: %v", err)
				}
			}
		}
	}()

	// collect child pids
	var pids []string

	// launch child procs
	for range maxProcs {
		cmd := exec.Command(os.Args[0], os.Args[1:]...) //nolint:gosec // It's fine to launch the same process again
		if testPreforkMaster {
			// When test prefork master,
			// just start the child process with a dummy cmd,
			// which will exit soon
			cmd = dummyCmd()
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		// add fiber prefork child flag into child proc env
		cmd.Env = append(os.Environ(),
			fmt.Sprintf("%s=%s", envPreforkChildKey, envPreforkChildVal),
		)

		if err = cmd.Start(); err != nil {
			return fmt.Errorf("failed to start a child prefork process, error: %w", err)
		}

		// store child process
		pid := cmd.Process.Pid
		children[pid] = cmd
		pids = append(pids, strconv.Itoa(pid))

		// execute fork hook
		if app.hooks != nil {
			if testOnPrefork {
				app.hooks.executeOnForkHooks(dummyPid)
			} else {
				app.hooks.executeOnForkHooks(pid)
			}
		}

		// notify master if child crashes
		go func() {
			channel <- child{pid: pid, err: cmd.Wait()}
		}()
	}

	// Run onListen hooks
	// Hooks have to be run here as different as non-prefork mode due to they should run as child or master
	app.runOnListenHooks(app.prepareListenData(addr, tlsConfig != nil, cfg))

	// Print startup message
	if !cfg.DisableStartupMessage {
		app.startupMessage(addr, tlsConfig != nil, ","+strings.Join(pids, ","), cfg)
	}

	// Print routes
	if cfg.EnablePrintRoutes {
		app.printRoutesMessage()
	}

	// return error if child crashes
	return (<-channel).err
}

// watchMaster watches child procs
func watchMaster() {
	if runtime.GOOS == "windows" {
		// finds parent process,
		// and waits for it to exit
		p, err := os.FindProcess(os.Getppid())
		if err == nil {
			_, _ = p.Wait() //nolint:errcheck // It is fine to ignore the error here
		}
		os.Exit(1) //nolint:revive // Calling os.Exit is fine here in the prefork
	}
	// if it is equal to 1 (init process ID),
	// it indicates that the master process has exited
	const watchInterval = 500 * time.Millisecond
	for range time.NewTicker(watchInterval).C {
		if os.Getppid() == 1 {
			os.Exit(1) //nolint:revive // Calling os.Exit is fine here in the prefork
		}
	}
}

var (
	dummyPid      = 1
	dummyChildCmd atomic.Value
)

// dummyCmd is for internal prefork testing
func dummyCmd() *exec.Cmd {
	command := "go"
	if storeCommand := dummyChildCmd.Load(); storeCommand != nil && storeCommand != "" {
		command = storeCommand.(string) //nolint:forcetypeassert,errcheck // We always store a string in here
	}
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", command, "version")
	}
	return exec.Command(command, "version")
}
