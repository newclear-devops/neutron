package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"neutron/internal"
	"neutron/internal/ccwork"
	"neutron/internal/model"
	"neutron/internal/notify"
	"neutron/internal/snippets"
)

//go:embed static/*
var staticFs embed.FS

var (
	safeParamKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func main() {
	config, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	if ttl := config.Kubernetes.EffectiveJobTtlSeconds(); ttl != nil {
		log.Printf("finished pipeline jobs are auto-deleted by Kubernetes after %s", time.Duration(*ttl)*time.Second)
		if d := time.Duration(*ttl) * time.Second; d < reconcileGracePeriod {
			// Below the grace period the K8s Job can disappear before the
			// reconciler reads it, stranding rows that never got a final report.
			log.Printf("WARNING: job-ttl-minutes is shorter than the reconciler grace period (%s); "+
				"jobs that never report a final status may be stuck as running forever", reconcileGracePeriod)
		}
	} else {
		log.Printf("finished pipeline jobs are kept indefinitely (job-ttl-minutes disabled); clean them up manually")
	}

	repo := internal.NewRepository(config)

	// Seed snippet cache from GitLab on startup (non-fatal on failure).
	if config.Snippets.RepoUrl != "" {
		if cb, ok := config.BaseConfig[config.Snippets.Platform]; ok {
			count, err := snippets.SyncSnippets(config.Snippets.RepoUrl, config.Snippets.Platform, config.Snippets.Ref, cb.Url, cb.Token, cb.SkipTLSVerify, repo)
			if err != nil {
				log.Printf("WARNING: initial snippet sync failed (serving cached data): %v", err)
			} else {
				log.Printf("initial snippet sync: %d snippets loaded from %s", count, config.Snippets.RepoUrl)
			}
		} else {
			log.Printf("WARNING: snippets platform %q not found in codebase config; skipping initial sync", config.Snippets.Platform)
		}
	}

	// Initialize notify client
	var notifyClient *notify.Client
	if config.Notify.Url != "" && config.Notify.CorpId != "" && config.Notify.AppId != "" {
		notifyClient = notify.NewClient(config.Notify.Url, config.Notify.CorpId, config.Notify.AppId, config.Notify.SkipTLSVerify)
	}

	// Initialize CCWork robot client
	ccworkRobot := ccwork.NewRobot(config.Notify.SkipTLSVerify)

	kubeConfig, err := rest.InClusterConfig()
	if err != nil {
		kubeConfig, err = clientcmd.BuildConfigFromFlags("", config.Kubernetes.KubeConfig)
		if err != nil {
			log.Fatalf("cannot build kube config: %v", err)
		}
	}
	clientSet, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		panic(err)
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()
	subStaticFs, err := fs.Sub(staticFs, "static")
	if err != nil {
		log.Fatalf("cannot load embedded static files: %v", err)
	}
	r.StaticFS("/static", http.FS(subStaticFs))

	// SPA: serve index.html for all non-API, non-static routes. The SPA is a
	// single embedded file with no versioned filename, so without validators a
	// browser happily keeps serving the copy it cached weeks ago and renders it
	// against an API it no longer matches. Hash the payload once into an ETag
	// and pair it with no-cache so every deploy is picked up.
	indexHTML, err := staticFs.ReadFile("static/index.html")
	if err != nil {
		log.Fatalf("cannot read embedded index.html: %v", err)
	}
	indexSum := sha256.Sum256(indexHTML)
	indexETag := `"` + hex.EncodeToString(indexSum[:]) + `"`
	r.NoRoute(func(c *gin.Context) {
		c.Header("ETag", indexETag)
		c.Header("Cache-Control", "no-cache, must-revalidate")
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	})

	server := NewServer(config, repo, clientSet, notifyClient, ccworkRobot)
	server.registerRoutes(r)
	// Close out jobs whose pods died without a final runner report
	// (checkout conflict, image pull failure, OOM) and notify their targets.
	server.startReconciler(context.Background(), 30*time.Second)

	// --- Snippet management ---

	r.GET("/api/snippets", func(c *gin.Context) {
		snippets, err := repo.ListSnippets()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"snippets": snippets})
	})

	r.GET("/api/snippets/:name", func(c *gin.Context) {
		name := c.Param("name")
		snippet, err := repo.GetSnippetByName(name)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "snippet not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"snippet": snippet})
	})

	// Refresh snippets from the configured GitLab project.
	r.POST("/api/snippets/refresh", func(c *gin.Context) {
		sc := config.Snippets
		if sc.RepoUrl == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "snippets repo not configured"})
			return
		}
		cb, ok := config.BaseConfig[sc.Platform]
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("snippets platform %q not found in codebase config", sc.Platform)})
			return
		}
		count, err := snippets.SyncSnippets(sc.RepoUrl, sc.Platform, sc.Ref, cb.Url, cb.Token, cb.SkipTLSVerify, repo)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "count": count})
	})

	// --- Default pipeline (fallback when a repo has no neutron.yaml) ---
	// GET is shared by the SPA editor and the pod-side runner callback.

	r.GET("/api/default-pipeline", func(c *gin.Context) {
		content, err := repo.GetSetting(internal.SettingDefaultPipeline)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"content": content})
	})

	r.PUT("/api/default-pipeline", func(c *gin.Context) {
		var req struct {
			Content string `json:"content"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		// Non-empty content must parse as a valid pipeline. Empty disables fallback.
		if strings.TrimSpace(req.Content) != "" {
			var pipeline model.Pipeline
			if err := yaml.Unmarshal([]byte(req.Content), &pipeline); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid pipeline yaml: %v", err)})
				return
			}
			if len(pipeline.Jobs) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "pipeline defines no jobs"})
				return
			}
		}
		if err := repo.SetSetting(internal.SettingDefaultPipeline, req.Content); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// Raw snippet endpoint for curl | bash / source <(curl)
	r.GET("/s/:name", func(c *gin.Context) {
		name := c.Param("name")
		snippet, err := repo.GetSnippetByName(name)
		if err != nil {
			c.String(http.StatusNotFound, "snippet not found")
			return
		}
		// Prepend query parameters as shell variable assignments (sorted for determinism)
		query := c.Request.URL.Query()
		keys := make([]string, 0, len(query))
		for k := range query {
			if safeParamKey.MatchString(k) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		var lines []string
		for _, key := range keys {
			values := query[key]
			if len(values) > 0 {
				escaped := strings.ReplaceAll(values[0], `"`, `\"`)
				lines = append(lines, fmt.Sprintf(`%s="%s";`, key, escaped))
			}
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, snippet.Content)
		c.String(http.StatusOK, strings.Join(lines, "\n"))
	})

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", config.Port),
		Handler: r,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("server forced shutdown: %v", err)
	}
	log.Println("server exited")
}
