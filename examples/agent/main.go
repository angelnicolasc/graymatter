// Package main shows the canonical GrayMatter integration pattern for a
// skill-based agent that calls the Anthropic Messages API.
//
// This is a self-contained demo: it defines a minimal Skill/Task/Project
// structure and shows exactly where GrayMatter plugs in — before and after
// the LLM call, in three lines total.
//
// Run:
//
//	ANTHROPIC_API_KEY=sk-... go run ./examples/agent
package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	graymatter "github.com/angelnicolasc/graymatter"
)

// Skill represents a named agent persona with its own system prompt.
type Skill struct {
	Name     string
	Identity string // system prompt
}

// Task is the unit of work the agent is asked to perform.
type Task struct {
	Description string
}

// Project holds path information for the current workspace.
type Project struct {
	Root string
}

func main() {
	project := Project{Root: "."}
	skill := Skill{
		Name:     "sales-closer",
		Identity: "You are an expert sales closer. You help close B2B SaaS deals.",
	}
	task := Task{
		Description: "Draft a follow-up email for Maria. She expressed interest but went quiet after the demo.",
	}

	// ── GrayMatter integration — 3 lines before the API call ─────────────────
	mem := graymatter.New(project.Root + "/.graymatter")
	defer mem.Close()
	if !mem.Healthy() {
		log.Fatalf("graymatter: %v", mem.Status().InitError)
	}

	ctx := context.Background()

	// Pre-populate with context from previous runs (simulates prior history).
	_ = mem.Remember(ctx, skill.Name, "Maria Rodriguez, VP Sales at Acme Corp. Demo was March 12.")
	_ = mem.Remember(ctx, skill.Name, "Maria's pain point: their current CRM doesn't integrate with Slack.")
	_ = mem.Remember(ctx, skill.Name, "Maria has budget approval. Deal size: $48k ARR.")
	_ = mem.Remember(ctx, skill.Name, "Maria went quiet after demo on March 12. No reply to follow-up on March 15.")

	// Recall relevant memory for this task.
	memCtx, err := mem.Recall(ctx, skill.Name, task.Description)
	if err != nil {
		log.Fatalf("recall: %v", err)
	}

	// Inject memory into the system prompt.
	systemContent := skill.Identity
	if len(memCtx) > 0 {
		systemContent += "\n\n## Memory\n" + strings.Join(memCtx, "\n")
	}

	fmt.Println("=== Memory context injected ===")
	for i, m := range memCtx {
		fmt.Printf("[%d] %s\n", i+1, m)
	}
	fmt.Println()

	// ── Anthropic Messages API call ───────────────────────────────────────────
	client := anthropic.NewClient()
	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5_20251001,
		MaxTokens: 512,
		System: []anthropic.TextBlockParam{
			{Text: systemContent},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(task.Description)),
		},
	})
	if err != nil {
		log.Fatalf("anthropic: %v", err)
	}

	output := ""
	if len(msg.Content) > 0 {
		output = msg.Content[0].Text
	}

	fmt.Println("=== Agent output ===")
	fmt.Println(output)

	// ── After run: store key observations ────────────────────────────────────
	keyFact := "Sent follow-up email to Maria on 2026-04-09. Subject: re: the Slack integration demo."
	if err := mem.Remember(ctx, skill.Name, keyFact); err != nil {
		log.Printf("warn: remember: %v", err)
	}

	fmt.Printf("\n[Stored: %q]\n", keyFact)
}
