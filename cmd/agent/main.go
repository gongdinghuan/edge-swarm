package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"

	"edgeswarm/internal/agent"
)

func main() {
	control := flag.String("control", "http://127.0.0.1:8080", "control HTTPS base URL; HTTP allowed only on loopback")
	model := flag.String("model", "", "exact served model identifier")
	backend := flag.String("backend", "http://127.0.0.1:8000/v1", "trusted local OpenAI-compatible backend base URL")
	probe := flag.Bool("probe-backend", true, "require the configured model in the backend /models response")
	backendKind := flag.String("backend-kind", "openai-compatible", "backend name reported to control, e.g. llama.cpp or vllm")
	gpu := flag.String("gpu", "", "GPU model reported to control (optional)")
	vram := flag.Int("vram-mib", 0, "GPU memory in MiB reported to control (optional)")
	check := flag.Bool("check", false, "check control readiness and the backend model without starting a worker session")
	flag.Parse()
	if *vram < 0 || len(*gpu) > 200 || len(*backendKind) > 100 {
		log.Fatal("invalid GPU or backend capability metadata")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	worker := agent.New(agent.Config{
		URL: *control, NodeID: os.Getenv("SWARM_NODE_ID"), Token: os.Getenv("SWARM_NODE_TOKEN"),
		Model: *model, BackendURL: *backend, BackendKey: os.Getenv("SWARM_BACKEND_KEY"),
		ProbeBackend: *probe, GPU: *gpu, VRAMMiB: *vram, BackendKind: *backendKind,
	})
	if *check {
		if err := worker.Check(ctx); err != nil {
			log.Fatal(err)
		}
		log.Print("control readiness and backend model checks passed; verify node enrollment in the console after starting")
		return
	}
	log.Print("agent starting: one execution slot; backend credentials remain local")
	if err := worker.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
