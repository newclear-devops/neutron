package main

import (
	"context"
	"embed"
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
)

//go:embed static/*
var staticFs embed.FS

var (
	snippetNameRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	safeParamKey     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func main() {
	config, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}

	repo := internal.NewRepository(config)

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

	// SPA: serve index.html for all non-API, non-static routes
	r.NoRoute(func(c *gin.Context) {
		data, err := staticFs.ReadFile("static/index.html")
		if err != nil {
			c.String(http.StatusInternalServerError, "SPA not found")
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
	})

	server := NewServer(config, repo, clientSet, notifyClient, ccworkRobot)
	server.registerRoutes(r)

	// --- Snippet management ---

	r.GET("/api/snippets", func(c *gin.Context) {
		snippets, err := repo.ListSnippets()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"snippets": snippets})
	})

	r.POST("/api/snippets", func(c *gin.Context) {
		var req struct {
			Name        string `json:"name"`
			Title       string `json:"title"`
			Content     string `json:"content"`
			Description string `json:"description"`
			Params      string `json:"params"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if req.Name == "" || req.Title == "" || req.Content == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name, title, and content are required"})
			return
		}
		// Validate name format: must be a safe URL slug
		if !snippetNameRegex.MatchString(req.Name) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name must be a valid slug (lowercase letters, digits, hyphens)"})
			return
		}
		// Check duplicate
		if _, err := repo.GetSnippetByName(req.Name); err == nil {
			c.JSON(http.StatusConflict, gin.H{"error": "snippet name already exists"})
			return
		}
		snippet := internal.Snippet{
			Name:        req.Name,
			Title:       req.Title,
			Content:     req.Content,
			Description: req.Description,
			Params:      req.Params,
		}
		if err := repo.CreateSnippet(snippet); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true, "name": req.Name})
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

	r.PATCH("/api/snippets/:name", func(c *gin.Context) {
		name := c.Param("name")
		if _, err := repo.GetSnippetByName(name); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "snippet not found"})
			return
		}
		var req map[string]interface{}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		updates := make(map[string]interface{})
		if v, ok := req["title"]; ok {
			if s, isStr := v.(string); isStr && s != "" {
				updates["title"] = s
			}
		}
		if v, ok := req["content"]; ok {
			if s, isStr := v.(string); isStr && s != "" {
				updates["content"] = s
			}
		}
		if v, ok := req["description"]; ok {
			updates["description"] = v
		}
		if v, ok := req["params"]; ok {
			updates["params"] = v
		}
		if len(updates) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
			return
		}
		if err := repo.UpdateSnippet(name, updates); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	r.DELETE("/api/snippets/:name", func(c *gin.Context) {
		name := c.Param("name")
		if _, err := repo.GetSnippetByName(name); err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "snippet not found"})
			return
		}
		if err := repo.DeleteSnippet(name); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"ok": true})
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
