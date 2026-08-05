package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"neutron/internal/model"
)

// loadConfig reads the YAML config file (path from NEUTRON_CONFIG, default
// ./config.yaml) and applies NEUTRON_* environment variable overrides.
func loadConfig() (model.Config, error) {
	var config model.Config
	configPath := os.Getenv("NEUTRON_CONFIG")
	if configPath == "" {
		configPath = "./config.yaml"
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return config, fmt.Errorf("cannot read config file: %w", err)
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, fmt.Errorf("cannot parse config file: %w", err)
	}

	applyEnvOverrides(&config)
	return config, nil
}

// envStr invokes set with the value of key when it is non-empty.
func envStr(key string, set func(string)) {
	if v := os.Getenv(key); v != "" {
		set(v)
	}
}

// envTrue invokes set when key is exactly "true".
func envTrue(key string, set func()) {
	if os.Getenv(key) == "true" {
		set()
	}
}

// applyCodebaseEnv applies URL/token/skip-TLS overrides for a single codebase
// entry from the NEUTRON_<PREFIX>_* environment variables.
func applyCodebaseEnv(config *model.Config, name, prefix string) {
	envStr(prefix+"_URL", func(v string) {
		cb := config.BaseConfig[name]
		cb.Url = v
		config.BaseConfig[name] = cb
	})
	envStr(prefix+"_TOKEN", func(v string) {
		cb := config.BaseConfig[name]
		cb.Token = v
		config.BaseConfig[name] = cb
	})
	envTrue(prefix+"_SKIP_TLS_VERIFY", func() {
		cb := config.BaseConfig[name]
		cb.SkipTLSVerify = true
		config.BaseConfig[name] = cb
	})
}

func applyEnvOverrides(config *model.Config) {
	envStr("NEUTRON_HOST", func(v string) {
		if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
			v = "http://" + v
		}
		config.Host = v
	})
	envStr("NEUTRON_PORT", func(v string) {
		if p, err := strconv.Atoi(v); err == nil {
			config.Port = p
		}
	})
	envStr("NEUTRON_DATABASE", func(v string) { config.Database = v })
	envStr("NEUTRON_LOG_URL", func(v string) { config.LogUrl = v })
	envStr("NEUTRON_KUBE_NAMESPACE", func(v string) { config.Kubernetes.Namespace = v })
	envStr("NEUTRON_KUBE_CONFIG", func(v string) { config.Kubernetes.KubeConfig = v })
	envStr("NEUTRON_GIT_PRIVATE_KEY", func(v string) { config.Kubernetes.GitPrivateKey = v })
	envStr("NEUTRON_INIT_IMAGE", func(v string) { config.Kubernetes.InitImage = v })
	envStr("NEUTRON_CHECKOUT_IMAGE", func(v string) { config.Kubernetes.CheckoutImage = v })
	envStr("NEUTRON_IMAGE_PULL_SECRETS", func(v string) {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				config.Kubernetes.ImagePullSecrets = append(config.Kubernetes.ImagePullSecrets, s)
			}
		}
	})

	// Global skip-TLS toggle for every configured codebase.
	envTrue("NEUTRON_SKIP_TLS_VERIFY", func() {
		for k, cb := range config.BaseConfig {
			cb.SkipTLSVerify = true
			config.BaseConfig[k] = cb
		}
	})

	applyCodebaseEnv(config, "GitLab", "NEUTRON_GITLAB")
	applyCodebaseEnv(config, "Codeup", "NEUTRON_CODEUP")
	envStr("NEUTRON_CODEUP_WEBHOOK_URL", func(v string) {
		cb := config.BaseConfig["Codeup"]
		cb.WebhookUrl = v
		config.BaseConfig["Codeup"] = cb
	})

	envStr("NEUTRON_NOTIFY_URL", func(v string) { config.Notify.Url = v })
	envStr("NEUTRON_NOTIFY_CORP_ID", func(v string) { config.Notify.CorpId = v })
	envStr("NEUTRON_NOTIFY_APP_ID", func(v string) { config.Notify.AppId = v })
	envTrue("NEUTRON_NOTIFY_SKIP_TLS_VERIFY", func() { config.Notify.SkipTLSVerify = true })
	envStr("NEUTRON_POD_API_URL", func(v string) { config.Kubernetes.PodApiUrl = v })

	envStr("NEUTRON_SNIPPETS_REPO_URL", func(v string) { config.Snippets.RepoUrl = v })
	envStr("NEUTRON_SNIPPETS_PLATFORM", func(v string) { config.Snippets.Platform = v })
	envStr("NEUTRON_SNIPPETS_REF", func(v string) { config.Snippets.Ref = v })
}
