package main

import (
	"embed"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

//go:embed all:frontend
var assets embed.FS

func main() {
	// 创建默认配置
	config := &Config{
		URL:      "ws://localhost:5000/ws",
		ClientID: "cli",
	}

	// 初始化 Ed25519 设备身份
	if err := config.EnsureDeviceIdentity(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: 初始化设备身份失败: %v\n", err)
		os.Exit(1)
	}

	// 启动 Wails 桌面应用
	wailsApp := NewWailsApp(config)

	err := wails.Run(&options.App{
		Title:     "OpenClaw Chat",
		Width:     960,
		Height:    680,
		MinWidth:  700,
		MinHeight: 500,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup:  wailsApp.startup,
		OnShutdown: wailsApp.shutdown,
		Bind: []interface{}{
			wailsApp,
		},
		Mac: &mac.Options{
			TitleBar: &mac.TitleBar{
				TitlebarAppearsTransparent: true,
				HideTitle:                 true,
				HideTitleBar:              false,
				FullSizeContent:           true,
				UseToolbar:                false,
			},
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}
