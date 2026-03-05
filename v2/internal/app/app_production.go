//go:build production

package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/wailsapp/wails/v2/internal/binding"
	"github.com/wailsapp/wails/v2/internal/frontend/desktop"
	"github.com/wailsapp/wails/v2/internal/frontend/dispatcher"
	"github.com/wailsapp/wails/v2/internal/frontend/runtime"
	"github.com/wailsapp/wails/v2/internal/logger"
	"github.com/wailsapp/wails/v2/internal/menumanager"
	"github.com/wailsapp/wails/v2/pkg/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options"
)

func (a *App) Run() error {
	err := a.frontend.Run(a.ctx)
	a.frontend.RunMainLoop()
	a.frontend.WindowClose()
	if a.shutdownCallback != nil {
		a.shutdownCallback(a.ctx)
	}
	return err
}

// rejectRequest 直接拒绝请求，返回 403 Forbidden。
func rejectRequest(w http.ResponseWriter) {
	w.WriteHeader(http.StatusForbidden)
}

// CreateApp creates the app!
func CreateApp(appoptions *options.App) (*App, error) {
	var err error

	ctx := context.Background()

	// Merge default options
	options.MergeDefaults(appoptions)

	debug := IsDebug()
	devtoolsEnabled := IsDevtoolsEnabled()
	ctx = context.WithValue(ctx, "debug", debug)
	ctx = context.WithValue(ctx, "devtoolsEnabled", devtoolsEnabled)

	// Set up logger
	myLogger := logger.New(appoptions.Logger)
	if IsDebug() {
		myLogger.SetLogLevel(appoptions.LogLevel)
	} else {
		myLogger.SetLogLevel(appoptions.LogLevelProduction)
	}
	ctx = context.WithValue(ctx, "logger", myLogger)
	ctx = context.WithValue(ctx, "obfuscated", IsObfuscated())

	// Preflight Checks
	err = PreflightChecks(appoptions, myLogger)
	if err != nil {
		return nil, err
	}

	// Create the menu manager
	menuManager := menumanager.NewManager()

	// Process the application menu
	if appoptions.Menu != nil {
		err = menuManager.SetApplicationMenu(appoptions.Menu)
		if err != nil {
			return nil, err
		}
	}

	// Create binding exemptions - Ugly hack. There must be a better way
	bindingExemptions := []interface{}{
		appoptions.OnStartup,
		appoptions.OnShutdown,
		appoptions.OnDomReady,
		appoptions.OnBeforeClose,
	}
	appBindings := binding.NewBindings(myLogger, appoptions.Bind, bindingExemptions, IsObfuscated(), appoptions.EnumBind)
	eventHandler := runtime.NewEvents(myLogger)
	ctx = context.WithValue(ctx, "events", eventHandler)
	// Attach logger to context
	if debug {
		ctx = context.WithValue(ctx, "buildtype", "debug")
	} else {
		ctx = context.WithValue(ctx, "buildtype", "production")
	}

	messageDispatcher := dispatcher.NewDispatcher(ctx, myLogger, appBindings, eventHandler, appoptions.ErrorFormatter)

	// Start HTTP server in production so the webview can load http://localhost:port (same as dev).
	var bindingsJSON string
	if !IsObfuscated() {
		var errBind error
		bindingsJSON, errBind = appBindings.ToJSON()
		if errBind != nil {
			return nil, errBind
		}
	} else {
		appBindings.DB().UpdateObfuscatedCallMap()
	}
	prodAssetServer, err := assetserver.NewAssetServerMainPage(bindingsJSON, appoptions, false, myLogger, runtime.RuntimeAssetsBundle)
	if err != nil {
		return nil, err
	}
	// Random token so only our app (with the URL we give it) can load assets; other clients get connection closed with no response.
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, err
	}
	assetToken := hex.EncodeToString(tokenBytes)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("_wails")
		referer := r.Header.Get("Referer")
		path := r.URL.Path
		isDoc := path == "" || path == "/" || strings.HasSuffix(path, ".html") || strings.HasSuffix(path, "/")
		// 子资源：允许 URL 带 token，或 Referer 同源（Referer 通常不含 query，不会带 _wails）
		sameOriginReferer := false
		if referer != "" && r.Host != "" {
			if refURL, err := url.Parse(referer); err == nil && refURL.Host == r.Host {
				sameOriginReferer = true
			}
		}
		allowed := got == assetToken || sameOriginReferer
		if isDoc {
			if got != assetToken {
				rejectRequest(w)
				return
			}
		} else if !allowed {
			rejectRequest(w)
			return
		}
		prodAssetServer.ServeHTTP(w, r)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	go func() {
		if err := http.Serve(listener, handler); err != nil && err != http.ErrServerClosed {
			myLogger.Error("Production asset HTTP server: %s", err)
		}
	}()
	// Pass token down via context; starturl carries the token so only our window can load it.
	ctx = context.WithValue(ctx, "assetservertoken", assetToken)
	startURL, _ := url.Parse("http://127.0.0.1:" + strconv.Itoa(port) + "/?_wails=" + assetToken)
	ctx = context.WithValue(ctx, "starturl", startURL)
	myLogger.Debug("Serving assets at http://127.0.0.1:%d (token-protected)", port)

	appFrontend := desktop.NewFrontend(ctx, appoptions, myLogger, appBindings, messageDispatcher)
	eventHandler.AddFrontend(appFrontend)

	ctx = context.WithValue(ctx, "frontend", appFrontend)
	result := &App{
		ctx:              ctx,
		frontend:         appFrontend,
		logger:           myLogger,
		menuManager:      menuManager,
		startupCallback:  appoptions.OnStartup,
		shutdownCallback: appoptions.OnShutdown,
		debug:            debug,
		devtoolsEnabled:  devtoolsEnabled,
		options:          appoptions,
	}

	return result, nil

}
