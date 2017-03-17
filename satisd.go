package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

type PackageInfo struct {
	packageName    string
	packageVersion string
	repositoryUrl  string
	repositoryType string
}

// Flags
var (
	satisPath  string
	configPath string
	repoPath   string

	listen string
)

// Operation vars
var (
	shouldGenerateConfig bool
	shouldGenerateRepo   bool
	runningGoroutines    = 0

	pendingUpdates map[string]*PackageInfo = make(map[string]*PackageInfo)

	updateMutex sync.Mutex
	configMutex sync.RWMutex
)

func init() {
	flag.StringVar(&satisPath, "satis", "", "The path to the satis binary (required)")
	flag.StringVar(&configPath, "config", "", "The path to the satis repo configuration file (required)")
	flag.StringVar(&repoPath, "repo", "", "The path to the satis repository (required)")

	flag.StringVar(&listen, "listen", ":8080", "The address to listen on")
}

func printHelp() {
	flag.PrintDefaults()
}

// Writes the satis configuration file upon receiving a signal
func configGenerator(abortChan chan bool) {
	runningGoroutines += 1
	defer func() {
		runningGoroutines -= 1
	}()

	var config map[string]interface{}
	for {
		// check if we're shutting down
		select {
		case <-abortChan:
			return
		default:
			break
		}

		// Have we been flagged for a config rebuild?
		if !shouldGenerateConfig {
			continue
		}

		// Read the existing config
		configMutex.RLock()
		data, err := ioutil.ReadFile(configPath)
		if err != nil {
			configMutex.RUnlock()
			fmt.Fprintf(os.Stderr, "Failed to load satis config file: %s", err)
			time.Sleep(time.Second * 5)
			continue
		}
		configMutex.RUnlock()
		// Decode the config
		err = json.Unmarshal(data, &config)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to decode satis config file: %s", err)
			time.Sleep(time.Second * 5)
			continue
		}

		var repositories []map[string]interface{}
		var packages map[string]interface{}

		// Create the keys if they don't exist,
		// but assume they're the correct type if they do exist
		if tmp, ok := config["repositories"]; !ok {
			repositories = make([]map[string]interface{}, 0, 1)
		} else {
			tmp2 := tmp.([]interface{})
			repositories = make([]map[string]interface{}, len(tmp2))
			for k, repo := range tmp2 {
				repositories[k] = repo.(map[string]interface{})
			}
		}
		if tmp, ok := config["require"]; !ok {
			packages = make(map[string]interface{})
		} else {
			packages = tmp.(map[string]interface{})
		}

		// Update the config
		updateMutex.Lock()
		for _, packageInfo := range pendingUpdates {
			var repoExists = false

			// Update the repo if it already exists
			for _, repo := range repositories {
				if repo["url"] == packageInfo.repositoryUrl {
					repo["type"] = packageInfo.repositoryType
					repoExists = true
					break
				}
			}

			// Create the repo if it doesn't exist
			if !repoExists {
				repositories = append(repositories, map[string]interface{}{
					"url":  packageInfo.repositoryUrl,
					"type": packageInfo.repositoryType,
				})
			}

			// Update the package version
			packages[packageInfo.packageName] = packageInfo.packageVersion
		}
		updateMutex.Unlock()

		// Write the config changes
		config["repositories"] = repositories
		config["require"] = packages

		data, err = json.MarshalIndent(config, "", "    ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to encode satis config file: %s", err)
			os.Exit(1)
		}

		configMutex.Lock()
		err = ioutil.WriteFile(configPath, data, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to write satis config file: %s", err)
			continue
		}
		configMutex.Unlock()

		log.Println("Generated config file for", len(pendingUpdates), "updates")
		pendingUpdates = make(map[string]*PackageInfo)

		// Update the worker flags to trigger a build
		shouldGenerateConfig = false
		shouldGenerateRepo = true
	}
}

// Generates the satis repositroy upon receiving a signal
func repoGenerator(abortChan chan bool) {
	runningGoroutines += 1
	defer func() {
		runningGoroutines -= 1
	}()

	for {
		// check if we're shutting down
		select {
		case <-abortChan:
			return
		default:
			break
		}

		// Check if we should generate the repo
		if !shouldGenerateRepo {
			continue
		}

		// Lock the config writer for an arbitrary amount of time on the off chance we want to write
		// to just as we launch satis
		configMutex.RLock()
		go func() {
			time.Sleep(time.Second * 5)
			configMutex.RUnlock()
		}()

		command := exec.Command(satisPath, "build", configPath, repoPath)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		command.Stdin = os.Stdin
		err := command.Run()

		if err != nil {
			log.Fatalln("failed to execute satis: %s", err)
		}

		shouldGenerateRepo = false
	}
}

func serveHttp(abortChan chan bool) {
	runningGoroutines += 1
	defer func() {
		runningGoroutines -= 1
	}()

	// Serve the repo config
	http.HandleFunc("/config.json", func(w http.ResponseWriter, r *http.Request) {
		configMutex.RLock()
		defer configMutex.RUnlock()
		http.ServeFile(w, r, configPath)
	})

	// Endpoint to force regeneration
	http.HandleFunc("/generate", func(w http.ResponseWriter, r *http.Request) {
		log.Println("HTTP triggered repo generation")

		shouldGenerateRepo = true

		w.Header().Add("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte("{\"success\": true}"))
	})

	// Endpoint to register a repo update
	http.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Content-Type", "application/json")

		params := r.URL.Query()
		var update = &PackageInfo{
			repositoryUrl:  params.Get("repo"),
			repositoryType: params.Get("repoType"),
			packageName:    params.Get("package"),
			packageVersion: params.Get("version"),
		}

		// Basic sanity checking
		if update.packageName == "" {
			w.WriteHeader(400)
			w.Write([]byte("{\"error\": \"missing package\"}"))
			return
		}

		if update.repositoryUrl == "" {
			w.WriteHeader(400)
			w.Write([]byte("{\"error\": \"missing repo\"}"))
			return
		}

		if update.repositoryType == "" {
			w.WriteHeader(400)
			w.Write([]byte("{\"error\": \"missing repoType\"}"))
			return
		}

		if update.packageVersion == "" {
			update.packageVersion = "*"
		}

		updateMutex.Lock()
		pendingUpdates[update.packageName] = update
		updateMutex.Unlock()

		shouldGenerateRepo = true
		w.WriteHeader(200)
		w.Write([]byte("{\"success\": true}"))
	})

	// Serve the repo
	fs := http.FileServer(http.Dir(repoPath))
	http.Handle("/", fs)

	var server = http.Server{Addr: listen}
	var errorChan = make(chan error)

	go func() {
		fmt.Println("listening on", listen)
		errorChan <- server.ListenAndServe()
	}()

	select {
	case err := <-errorChan:
		log.Fatalln("HTTP listener error:", err)
		break
	case <-abortChan:
		server.Close()
		break
	}

}

func main() {
	fmt.Println("satisd - dynamic satis repository generator daemon")
	flag.Parse()

	if satisPath == "" || configPath == "" || repoPath == "" {
		printHelp()
		return
	}

	// Check that the files exist
	if _, err := os.Stat(satisPath); os.IsNotExist(err) {
		log.Fatalln("satis binary not found at", satisPath)
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		log.Fatalln("satis configuration not found at", configPath)
	}

	// Perform a basic sanity check on the config
	var config map[string]interface{}
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load satis config file: %s", err)
		os.Exit(1)
	}
	err = json.Unmarshal(data, &config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to decode satis config file: %s", err)
		os.Exit(1)
	}

	// Spin up the worker goroutines
	shutdownChannel := make(chan bool)
	go configGenerator(shutdownChannel)
	go repoGenerator(shutdownChannel)
	go serveHttp(shutdownChannel)

	// Catch signals
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	// We've got a kill signal? - do a clean shutdown
	sig := <-sigs
	fmt.Println("Received signal", sig)
	close(shutdownChannel)

	// Wait for the goroutines to exit
	for {
		if runningGoroutines == 0 {
			return
		}
		runtime.Gosched()
	}
}
