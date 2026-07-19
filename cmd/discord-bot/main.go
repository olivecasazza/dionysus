// Package main is the discord-bot entrypoint. It runs as its own
// Deployment (separate from the controller manager) and answers Discord
// interaction webhooks synchronously over HTTP.
//
// Required env:
//
//	DISCORD_PUBLIC_KEY  hex Ed25519 public key for request verification
//	DISCORD_APP_ID      application ID (for command registration)
//	DISCORD_BOT_TOKEN   bot token (Authorization: Bot …)
//
// Optional:
//
//	DISCORD_NAMESPACE   restrict reads to one namespace (empty = all)
//
// The bot self-registers its slash commands at startup against the
// Discord HTTP API. Re-registration is idempotent — Discord diffs by
// command name+options and only changes what differs.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	gamesv1alpha1 "github.com/olivecasazza/dionysus/api/v1alpha1"
	"github.com/olivecasazza/dionysus/internal/discord"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(gamesv1alpha1.AddToScheme(scheme))
}

func main() {
	var bind string
	flag.StringVar(&bind, "bind-address", ":8080", "Address the HTTP server binds to.")
	flag.Parse()

	logger := zap.New()
	setupLog := logger.WithName("setup")

	publicKey := os.Getenv("DISCORD_PUBLIC_KEY")
	appID := os.Getenv("DISCORD_APP_ID")
	botToken := os.Getenv("DISCORD_BOT_TOKEN")
	namespace := os.Getenv("DISCORD_NAMESPACE")
	if publicKey == "" || appID == "" || botToken == "" {
		setupLog.Error(nil, "DISCORD_PUBLIC_KEY, DISCORD_APP_ID, DISCORD_BOT_TOKEN must all be set")
		os.Exit(1)
	}

	// controller-runtime client. In-cluster when KUBERNETES_SERVICE_HOST
	// is set; kubeconfig otherwise (local development).
	cfg, err := config.GetConfig()
	if err != nil {
		setupLog.Error(err, "get kubeconfig")
		os.Exit(1)
	}
	k8s, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		setupLog.Error(err, "build k8s client")
		os.Exit(1)
	}

	// Register slash commands. Failures are non-fatal — the bot can
	// still answer Ping handshakes even if registration wedges, which
	// keeps the interactions URL healthy in Discord's eyes.
	if err := registerCommands(appID, botToken); err != nil {
		setupLog.Error(err, "register slash commands (non-fatal)")
	} else {
		setupLog.Info("registered slash commands", "appId", appID)
	}

	bot := discord.NewBot(publicKey, k8s, namespace)
	mux := http.NewServeMux()
	mux.Handle("/interactions", bot)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              bind,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	go func() {
		setupLog.Info("discord-bot listening", "bind", bind)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			setupLog.Error(err, "http server")
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	setupLog.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		setupLog.Error(err, "graceful shutdown failed")
	}
}

// registerCommands PUTs the slash command tree to Discord. PUT replaces
// the whole application command set, so removed commands on our side
// disappear on the next reconcile.
func registerCommands(appID, botToken string) error {
	commands := discord.Commands()
	body, err := json.Marshal(commands)
	if err != nil {
		return fmt.Errorf("marshal commands: %w", err)
	}
	url := fmt.Sprintf("https://discord.com/api/v10/applications/%s/commands", appID)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+botToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("PUT commands: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord PUT commands: %s: %s", resp.Status, string(respBody))
	}
	return nil
}
