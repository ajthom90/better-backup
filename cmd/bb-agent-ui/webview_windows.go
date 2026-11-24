package main

import "github.com/wailsapp/go-webview2/webviewloader"

// webviewAvailable reports whether the WebView2 runtime is installed.
// Windows 11 and current Windows 10 have it; Windows Server 2016 and
// 2019 do not unless someone installed it.
func webviewAvailable() bool {
	v, err := webviewloader.GetAvailableCoreWebView2BrowserVersionString("")
	return err == nil && v != ""
}
