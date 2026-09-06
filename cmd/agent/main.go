package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"rdpulse/internal/agent"
	"rdpulse/internal/config"
	"rdpulse/internal/gui"
	"rdpulse/internal/service"
)

func usage() {
	fmt.Printf("Usage: %s <command> [options]\n\n", filepath.Base(os.Args[0]))
	fmt.Println("Commands:")
	fmt.Println("  gui         Run agent interactive desktop GUI")
	fmt.Println("  connect     Connect to a remote target device as Controller (P2P + mstsc)")
	fmt.Println("  run         Run agent interactively in foreground (Controlled mode)")
	fmt.Println("  service     Run under the Windows Service Control Manager")
	fmt.Println("  install     Install agent as a Windows Service")
	fmt.Println("  uninstall   Uninstall the Windows Service")
	fmt.Println("  start       Start the installed Windows Service")
	fmt.Println("  stop        Stop the running Windows Service")
	fmt.Println("  status      Query the Windows Service status")
	fmt.Println("\nOptions:")
	fmt.Println("  --config    Path to agent config YAML (default: ~/.rdpulse/agent.yaml)")
	fmt.Println("  --proxy     Local proxy bind address (default: 127.0.0.1:13389)")
	fmt.Println("  --mstsc     Auto launch mstsc on connect (default: true)")
	fmt.Println("  --minimized Start GUI minimized to system tray")
}

func main() {
	defaultCfg := config.DefaultAgentConfigPath()

	if len(os.Args) < 2 {
		if service.IsWindowsService() {
			runServiceWithConfig(defaultCfg)
			return
		}
		// Default to GUI mode when double clicked or executed without arguments
		runGUI(defaultCfg, false)
		return
	}

	cmd := os.Args[1]
	switch cmd {
	case "gui":
		fs := flag.NewFlagSet("gui", flag.ContinueOnError)
		cfgPath := fs.String("config", defaultCfg, "path to agent config file")
		minimized := fs.Bool("minimized", false, "start minimized to system tray")
		tray := fs.Bool("tray", false, "start minimized to system tray")
		_ = fs.Parse(os.Args[2:])
		runGUI(*cfgPath, *minimized || *tray)
	case "--minimized", "-minimized", "--tray", "-tray":
		fs := flag.NewFlagSet("gui", flag.ContinueOnError)
		cfgPath := fs.String("config", defaultCfg, "path to agent config file")
		minimized := fs.Bool("minimized", true, "start minimized to system tray")
		tray := fs.Bool("tray", true, "start minimized to system tray")
		_ = fs.Parse(os.Args[1:])
		runGUI(*cfgPath, *minimized || *tray)
	case "connect":
		fs := flag.NewFlagSet("connect", flag.ExitOnError)
		cfgPath := fs.String("config", defaultCfg, "path to agent config file")
		proxyAddr := fs.String("proxy", "127.0.0.1:13389", "local proxy bind address")
		autoMstsc := fs.Bool("mstsc", true, "automatically launch mstsc")

		// Separate positional targetID from flags so either order works:
		// "connect <targetID> --config ..." or "connect --config ... <targetID>"
		rawArgs := os.Args[2:]
		var targetID string
		var flagArgs []string
		for i := 0; i < len(rawArgs); i++ {
			arg := rawArgs[i]
			if strings.HasPrefix(arg, "-") {
				flagArgs = append(flagArgs, arg)
				// If this flag takes an argument as the next element
				if !strings.Contains(arg, "=") && (arg == "-config" || arg == "--config" || arg == "-proxy" || arg == "--proxy") && i+1 < len(rawArgs) && !strings.HasPrefix(rawArgs[i+1], "-") {
					i++
					flagArgs = append(flagArgs, rawArgs[i])
				}
			} else if targetID == "" {
				targetID = arg
			} else {
				flagArgs = append(flagArgs, arg)
			}
		}

		_ = fs.Parse(flagArgs)
		if targetID == "" {
			targetID = fs.Arg(0)
		}
		if targetID == "" {
			fmt.Println("Error: target_device_id is required. Usage: rdp-agent connect <target_device_id> [options]")
			os.Exit(1)
		}
		runController(*cfgPath, targetID, *proxyAddr, *autoMstsc)
	case "install":
		fs := flag.NewFlagSet("install", flag.ExitOnError)
		cfgPath := fs.String("config", defaultCfg, "path to agent config file")
		_ = fs.Parse(os.Args[2:])
		if err := service.Install(*cfgPath); err != nil {
			log.Fatalf("Failed to install service: %v", err)
		}
		fmt.Println("Service installed successfully.")

	case "uninstall":
		if err := service.Uninstall(); err != nil {
			log.Fatalf("Failed to uninstall service: %v", err)
		}
		fmt.Println("Service uninstalled successfully.")

	case "start":
		if err := service.Start(); err != nil {
			log.Fatalf("Failed to start service: %v", err)
		}
		fmt.Println("Service started successfully.")

	case "stop":
		if err := service.Stop(); err != nil {
			log.Fatalf("Failed to stop service: %v", err)
		}
		fmt.Println("Service stopped successfully.")

	case "status":
		st, err := service.Status()
		if err != nil {
			log.Fatalf("Failed to query status: %v", err)
		}
		fmt.Printf("Service Status: %s\n", st)

	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		cfgPath := fs.String("config", defaultCfg, "path to agent config file")
		_ = fs.Parse(os.Args[2:])
		runForeground(*cfgPath)

	case "service":
		fs := flag.NewFlagSet("service", flag.ExitOnError)
		cfgPath := fs.String("config", defaultCfg, "path to agent config file")
		_ = fs.Parse(os.Args[2:])
		runServiceWithConfig(*cfgPath)

	case "help", "--help", "-h":
		usage()

	default:
		// Check if any flag indicates GUI minimized / tray mode
		isGUI := false
		for _, arg := range os.Args[1:] {
			if arg == "--minimized" || arg == "-minimized" || arg == "--tray" || arg == "-tray" || arg == "gui" {
				isGUI = true
				break
			}
		}
		if isGUI {
			fs := flag.NewFlagSet("gui", flag.ContinueOnError)
			cfgPath := fs.String("config", defaultCfg, "path to agent config file")
			minimized := fs.Bool("minimized", false, "start minimized to system tray")
			tray := fs.Bool("tray", false, "start minimized to system tray")
			_ = fs.Parse(os.Args[1:])
			runGUI(*cfgPath, *minimized || *tray)
			return
		}

		// If this is the dedicated GUI binary (e.g. rdp-agent-gui.exe), always run GUI
		if strings.Contains(strings.ToLower(filepath.Base(os.Args[0])), "gui") {
			fs := flag.NewFlagSet("gui", flag.ContinueOnError)
			cfgPath := fs.String("config", defaultCfg, "path to agent config file")
			minimized := fs.Bool("minimized", false, "start minimized to system tray")
			tray := fs.Bool("tray", false, "start minimized to system tray")
			_ = fs.Parse(os.Args[1:])
			runGUI(*cfgPath, *minimized || *tray)
			return
		}

		// Check if first arg is a flag like --config for foreground run
		if len(cmd) > 0 && cmd[0] == '-' {
			fs := flag.NewFlagSet("run", flag.ExitOnError)
			cfgPath := fs.String("config", defaultCfg, "path to agent config file")
			_ = fs.Parse(os.Args[1:])
			runForeground(*cfgPath)
			return
		}
		usage()
	}
}

func runServiceWithConfig(defaultConfig string) {
	err := service.RunService(func(ctx context.Context) error {
		cfg, err := config.LoadAgentConfig(defaultConfig)
		if err != nil {
			return fmt.Errorf("load config failed: %w", err)
		}
		client := agent.NewClient(cfg)
		return client.Run(ctx)
	})
	if err != nil {
		log.Fatalf("Agent service failed: %v", err)
	}
}

func runForeground(configPath string) {
	log.Printf("[RDPulse Agent] Starting in foreground with config: %s", configPath)

	cfg, err := config.LoadAgentConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load agent config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[RDPulse Agent] Stopping...")
		cancel()
	}()

	client := agent.NewClient(cfg)
	if err := client.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("[RDPulse Agent] Stopped with error: %v", err)
	}
}

func runController(configPath, targetID, proxyAddr string, autoMstsc bool) {
	log.Printf("[RDPulse Controller] Initializing connection to target %s...", targetID)

	cfg, err := config.LoadAgentConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Println("[RDPulse Controller] Shutting down...")
		cancel()
	}()

	client := agent.NewClient(cfg)
	proxy, pathMgr, err := client.ConnectTarget(ctx, targetID, proxyAddr, autoMstsc)
	if err != nil {
		log.Fatalf("[RDPulse Controller] Connection failed: %v", err)
	}
	defer proxy.Close()
	defer pathMgr.Close()

	log.Printf("[RDPulse Controller] Session established! Local proxy: %s", proxy.Addr())
	log.Printf("[RDPulse Controller] Path Status: TCP=%s, UDP=%s", pathMgr.TCPPath(), pathMgr.UDPPath())

	<-ctx.Done()
}

func runGUI(configPath string, startMinimized bool) {
	log.Printf("[RDPulse Agent] Starting Native Windows Desktop GUI with config: %s (minimized=%v)", configPath, startMinimized)
	gui.RunApp(configPath, startMinimized)
	log.Println("[RDPulse Agent] Desktop GUI closed.")
}
