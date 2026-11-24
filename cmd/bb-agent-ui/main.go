// bb-agent-ui is the better-backup desktop app: a window on the agent
// that runs in the background. It connects the machine to a backup
// server, shows how the backups are going and starts one.
//
// The window shows the page of package agentui, which talks to the
// agent over its local control channel; the app itself holds no state
// and needs no privileges beyond being allowed to use that channel.
//
// Build it with the tags "desktop,production" (and cgo, except on
// Windows); see packaging/. Without them it only prints how to.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/ajthom90/better-backup/internal/agent"
	"github.com/ajthom90/better-backup/internal/agentctl"
	"github.com/ajthom90/better-backup/internal/agentui"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("bb-agent-ui", agent.Version, runtime.GOOS+"/"+runtime.GOARCH)
		return
	}
	ctl := agentctl.Dial(agentctl.SocketPath(""))
	// Without a web view to show the page in (Windows Server 2016 and
	// 2019 have none), show it in the browser instead. This program
	// then stays, unseen, for as long as the page is open.
	if !webviewAvailable() {
		err := agentui.Local(context.Background(), ctl, 10*time.Minute, func(addr string) { agentui.OpenBrowser(addr) })
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	// The web view keeps its cache here; nothing of ours is in it.
	data := ""
	if dir, err := os.UserCacheDir(); err == nil {
		data = filepath.Join(dir, "better-backup", "webview")
	}
	err := wails.Run(&options.App{
		Title:     "Better Backup",
		Width:     580,
		Height:    740,
		MinWidth:  440,
		MinHeight: 480,
		AssetServer: &assetserver.Options{
			Handler: agentui.Handler(agentui.Options{Control: ctl, App: true}),
		},
		// Opening the app again brings the window forward.
		SingleInstanceLock: &options.SingleInstanceLock{UniqueId: "com.github.ajthom90.better-backup.agent-ui"},
		Mac: &mac.Options{
			About: &mac.AboutInfo{Title: "Better Backup", Message: "Version " + agent.Version + "\nOpen-source backups.\nhttps://github.com/ajthom90/better-backup"},
		},
		Windows: &windows.Options{WebviewUserDataPath: data},
		Linux:   &linux.Options{ProgramName: "better-backup"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
