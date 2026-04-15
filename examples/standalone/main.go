// Package main demonstrates GrayMatter with a bare Anthropic Messages API call.
//
// Run:
//
//	ANTHROPIC_API_KEY=sk-... go run ./examples/standalone
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	graymatter "github.com/angelnicolasc/graymatter"
)

const agentID = "standalone-demo"

func main() {
	// ── 1. Open memory ─────────────────────────────────────────────────────────
	mem := graymatter.New(".graymatter-demo")
	defer mem.Close()
	if !mem.Healthy() {
		log.Fatalf("graymatter: %v", mem.Status().InitError)
	}

	ctx := context.Background()

	// ── 2. Pre-seed a couple of facts (simulates previous runs) ───────────────
	_ = mem.Remember(ctx, agentID, "User's name is Alex. They prefer concise, bullet-point answers.")
	_ = mem.Remember(ctx, agentID, "Alex is building a sales automation tool in Go. Deadline: Q2 2026.")
	_ = mem.Remember(ctx, agentID, "Alex gets frustrated when responses are too long or repeat context.")

	// ── 3. Recall relevant context for the current task ────────────────────────
	task := "How should I structure the outreach sequence for a new lead?"

	memCtx, err := mem.Recall(ctx, agentID, task)
	if err != nil {
		log.Fatalf("recall: %v", err)
	}

	// ── 4. Build system prompt with injected memory ────────────────────────────
	systemPrompt := "You are a helpful sales strategy assistant."
	if len(memCtx) > 0 {
		systemPrompt += "\n\n## Memory\n" + strings.Join(memCtx, "\n")
	}

	fmt.Println("=== System prompt (with memory) ===")
	fmt.Println(systemPrompt)
	fmt.Println("\n=== User task ===")
	fmt.Println(task)
	fmt.Println("\n=== Calling Anthropic API ===")

	// ── 5. Call Anthropic Messages API ────────────────────────────────────────
	client := anthropic.NewClient()
	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5_20251001,
		MaxTokens: 512,
		System: []anthropic.TextBlockParam{
			{Text: systemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(task)),
		},
	})
	if err != nil {
		log.Fatalf("anthropic: %v", err)
	}

	response := ""
	if len(msg.Content) > 0 {
		response = msg.Content[0].Text
	}

	fmt.Println("\n=== Response ===")
	fmt.Println(response)

	// ── 6. Store the agent's observation from this run ─────────────────────────
	observation := "Alex asked about outreach sequencing for a new lead."
	_ = mem.Remember(ctx, agentID, observation)

	fmt.Printf("\n[Memory stored: %q]\n", observation)
	fmt.Printf("Token efficiency: memory context = %d tokens vs full history injection.\n",
		len(strings.Join(memCtx, " "))/4)
}
