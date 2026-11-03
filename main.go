package main

import (
	"fmt"
	"os"
)

const appVersion = "0.6.0"

// toolsVersion: the Windows ContextSaver release whose tools this build matches.
const toolsVersion = "1.17.1"

func main() {
	initApp()
	args := os.Args[1:]
	if len(args) == 0 {
		consoleMenu()
		return
	}
	switch args[0] {
	case "start":
		if len(args) > 1 {
			exitAfterPause(consoleStartModel(args[1]))
		}
		exitAfterPause(consoleStartLocalAI())
	case "stop":
		exitAfterPause(consoleStopLocalAI())
	case "model":
		exitAfterPause(consoleModel(args[1:]))
	case "approvals":
		os.Exit(consoleApprovals(args[1:]))
	case "mcp":
		for i := 1; i+1 < len(args); i++ {
			if args[i] == "--for" {
				setProfile(args[i+1])
			}
		}
		serveMCP(os.Stdin, os.Stdout)
	case "run":
		if len(args) < 2 {
			fmt.Println(`usage: contextsaver run "<saved job name>"`)
			os.Exit(2)
		}
		os.Exit(consoleRunSaved(args[1]))
	case "install":
		consoleInstall()
	case "uninstall":
		consoleUninstall()
	case "helper":
		os.Exit(serveElevatedHelper())
	case "install-helper":
		consoleInstallHelper()
	case "uninstall-helper":
		consoleUninstallHelper()
	case "setup":
		consoleSetup()
	case "refresh-helper":
		refreshHelper()
	case "version":
		fmt.Println("contextsaver", appVersion, "(Linux; tools of ContextSaver "+toolsVersion+")")
	case "chat":
		os.Exit(consoleChat(args[1:]))
	case "terminal":
		terminal()
	case "macro":
		os.Exit(runMacroCLI(args[1:]))
	default:
		fmt.Println("unknown command:", args[0])
		os.Exit(2)
	}
}

func exitAfterPause(code int) {
	pauseIfAsked()
	os.Exit(code)
}
