// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// reviewConfig holds LLM configuration for change review.
var reviewConfig struct {
	url   string // OpenAI-compatible API base URL
	key   string // API key
	model string // model name
}

// reviewEnabled returns true if the LLM review feature is configured.
func reviewEnabled() bool {
	return reviewConfig.url != "" && reviewConfig.key != ""
}

const reviewSystemPrompt = `You are a change-risk reviewer for a fleet management system.

Your task is to review a proposed action before it runs on managed nodes and identify:
- unintended consequences
- dangerous scope
- destructive behavior
- privilege escalation
- typo-like mistakes
- ambiguous targeting
- suspicious file paths or shell constructs
- irreversible operations
- missing safeguards
- anything that looks inconsistent with the operator's likely intent

Do not assume the action is safe just because it is syntactically valid.
Be conservative. Prefer flagging uncertainty over silently approving risky actions.

You are not the final decision maker.
You must not say "safe" unless the action is low risk and narrowly scoped.
If details are missing, say so explicitly.

Output JSON only with this schema:

{
  "summary": "short plain-English assessment",
  "risk_level": "low|medium|high|critical",
  "should_block_for_confirmation": true,
  "findings": [
    {
      "severity": "low|medium|high|critical",
      "category": "scope|destructive|privilege|typo|targeting|shell|file-write|network|service-impact|secrets|rollback|other",
      "message": "specific concern",
      "evidence": "quote or field from the request",
      "suggested_check": "what the operator should verify"
    }
  ],
  "possible_typos": [
    {
      "input": "original text",
      "reason": "why it looks wrong",
      "possible_intent": "likely intended text"
    }
  ],
  "questions_for_operator": [
    "short clarification question"
  ],
  "recommended_confirmation_message": "one concise confirmation message shown to the operator"
}`

// reviewAction holds all the context for a change review request.
type reviewAction struct {
	ActionType  string `json:"action_type"` // exec, playbook, deploy
	Command     string `json:"command,omitempty"`
	ScriptName  string `json:"script_name,omitempty"`
	ScriptBody  string `json:"script_content,omitempty"`
	TargetQuery string `json:"target_query,omitempty"`
	TargetCount int    `json:"resolved_target_count,omitempty"`
	Targets     string `json:"sample_targets,omitempty"`
	Become      bool   `json:"requires_privilege_escalation"`
	BecomeUser  string `json:"privilege_user,omitempty"`

	// Playbook fields.
	PlaybookPath  string            `json:"playbook_path,omitempty"`
	PlaybookFiles map[string]string `json:"playbook_files,omitempty"` // path → content
	Module        string            `json:"module,omitempty"`
	ModuleArgs    string            `json:"module_args,omitempty"`
	ExtraArgs     string            `json:"extra_args,omitempty"`

	// Deploy fields.
	PackagePath    string `json:"package_path,omitempty"`
	InstallCommand string `json:"install_command,omitempty"`
	DestPath       string `json:"dest_path,omitempty"`
}

// reviewResult is the parsed LLM response.
type reviewResult struct {
	Summary                 string          `json:"summary"`
	RiskLevel               string          `json:"risk_level"`
	ShouldBlock             bool            `json:"should_block_for_confirmation"`
	Findings                []reviewFinding `json:"findings"`
	PossibleTypos           []reviewTypo    `json:"possible_typos"`
	QuestionsForOperator    []string        `json:"questions_for_operator"`
	RecommendedConfirmation string          `json:"recommended_confirmation_message"`
}

type reviewFinding struct {
	Severity       string `json:"severity"`
	Category       string `json:"category"`
	Message        string `json:"message"`
	Evidence       string `json:"evidence"`
	SuggestedCheck string `json:"suggested_check"`
}

type reviewTypo struct {
	Input          string `json:"input"`
	Reason         string `json:"reason"`
	PossibleIntent string `json:"possible_intent"`
}

// runReview sends the action to the LLM for risk analysis and prompts
// the operator for confirmation if needed. Returns nil to proceed,
// or an error to abort.
func runReview(action reviewAction) error {
	if !reviewEnabled() {
		return nil
	}

	fmt.Fprintf(os.Stderr, "Reviewing action with %s...\n", reviewConfig.model)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := callLLM(ctx, action)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: LLM review failed: %v\nProceeding without review.\n\n", err)
		return nil
	}

	printReview(result)

	if result.ShouldBlock {
		msg := result.RecommendedConfirmation
		if msg == "" {
			msg = "Proceed with this action?"
		}
		fmt.Fprintf(os.Stderr, "\n%s [y/N] ", msg)
		var answer string
		fmt.Scanln(&answer)
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			return fmt.Errorf("aborted by operator")
		}
		fmt.Fprintln(os.Stderr)
	}

	return nil
}

func printReview(r *reviewResult) {
	// Risk level with color.
	var riskColor string
	switch r.RiskLevel {
	case "low":
		riskColor = "\033[32m" // green
	case "medium":
		riskColor = "\033[33m" // yellow
	case "high":
		riskColor = "\033[31m" // red
	case "critical":
		riskColor = "\033[1;31m" // bold red
	default:
		riskColor = ""
	}
	fmt.Fprintf(os.Stderr, "\nRisk: %s%s\033[0m — %s\n", riskColor, strings.ToUpper(r.RiskLevel), r.Summary)

	if len(r.Findings) > 0 {
		fmt.Fprintf(os.Stderr, "\nFindings:\n")
		for _, f := range r.Findings {
			fmt.Fprintf(os.Stderr, "  [%s] %s: %s\n", f.Severity, f.Category, f.Message)
			if f.Evidence != "" {
				fmt.Fprintf(os.Stderr, "        evidence: %s\n", f.Evidence)
			}
			if f.SuggestedCheck != "" {
				fmt.Fprintf(os.Stderr, "        check: %s\n", f.SuggestedCheck)
			}
		}
	}

	if len(r.PossibleTypos) > 0 {
		fmt.Fprintf(os.Stderr, "\nPossible typos:\n")
		for _, t := range r.PossibleTypos {
			fmt.Fprintf(os.Stderr, "  %q — %s (did you mean %q?)\n", t.Input, t.Reason, t.PossibleIntent)
		}
	}

	if len(r.QuestionsForOperator) > 0 {
		fmt.Fprintf(os.Stderr, "\nQuestions:\n")
		for _, q := range r.QuestionsForOperator {
			fmt.Fprintf(os.Stderr, "  - %s\n", q)
		}
	}
}

// callLLM sends the review request to an LLM API.
// Auto-detects Anthropic's native API vs OpenAI-compatible format.
func callLLM(ctx context.Context, action reviewAction) (*reviewResult, error) {
	userPrompt := buildReviewPrompt(action)

	var content string
	var err error
	if llmIsAnthropic(reviewConfig.url) {
		content, err = callAnthropic(ctx, userPrompt)
	} else {
		content, err = callOpenAICompat(ctx, userPrompt)
	}
	if err != nil {
		return nil, err
	}

	return parseReviewResponse(content)
}

// callAnthropic calls Anthropic's native /v1/messages API.
func callAnthropic(ctx context.Context, userPrompt string) (string, error) {
	data, err := llmRequest(ctx, reviewConfig.url, reviewConfig.key, map[string]any{
		"model":      reviewConfig.model,
		"max_tokens": 4096,
		"system":     reviewSystemPrompt,
		"messages": []map[string]string{
			{"role": "user", "content": userPrompt},
		},
	})
	if err != nil {
		return "", err
	}

	var anthropicResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data, &anthropicResp); err != nil {
		return "", fmt.Errorf("parse Anthropic response: %w", err)
	}
	if len(anthropicResp.Content) == 0 {
		return "", fmt.Errorf("Anthropic returned no content")
	}
	return anthropicResp.Content[0].Text, nil
}

// callOpenAICompat calls an OpenAI-compatible /v1/chat/completions API.
func callOpenAICompat(ctx context.Context, userPrompt string) (string, error) {
	data, err := llmRequest(ctx, reviewConfig.url, reviewConfig.key, map[string]any{
		"model": reviewConfig.model,
		"messages": []map[string]string{
			{"role": "system", "content": reviewSystemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"max_tokens": 4096,
	})
	if err != nil {
		return "", err
	}

	var llmResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &llmResp); err != nil {
		return "", fmt.Errorf("parse LLM response: %w", err)
	}
	if len(llmResp.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}
	return llmResp.Choices[0].Message.Content, nil
}

// parseReviewResponse extracts the JSON review result from LLM output.
func parseReviewResponse(content string) (*reviewResult, error) {

	// Strip markdown code fences if present.
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		lines := strings.SplitN(content, "\n", 2)
		if len(lines) > 1 {
			content = lines[1]
		}
		if idx := strings.LastIndex(content, "```"); idx >= 0 {
			content = content[:idx]
		}
		content = strings.TrimSpace(content)
	}

	var result reviewResult
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		return nil, fmt.Errorf("parse LLM JSON: %w (raw: %s)", err, content[:min(200, len(content))])
	}

	return &result, nil
}

func buildReviewPrompt(a reviewAction) string {
	var sb strings.Builder

	sb.WriteString("Review this proposed managed-node action.\n\n")

	sb.WriteString(fmt.Sprintf("Action type: %s\n", a.ActionType))
	if a.TargetQuery != "" {
		sb.WriteString(fmt.Sprintf("Target query: %s\n", a.TargetQuery))
	}
	if a.TargetCount > 0 {
		sb.WriteString(fmt.Sprintf("Resolved target count: %d\n", a.TargetCount))
	}
	if a.Targets != "" {
		sb.WriteString(fmt.Sprintf("Sample targets: %s\n", a.Targets))
	}
	sb.WriteString(fmt.Sprintf("Requires privilege escalation: %v\n", a.Become))
	if a.BecomeUser != "" {
		sb.WriteString(fmt.Sprintf("Privilege user: %s\n", a.BecomeUser))
	}

	if a.Command != "" {
		sb.WriteString(fmt.Sprintf("\nCommand: %s\n", a.Command))
	}

	if a.ScriptName != "" {
		sb.WriteString(fmt.Sprintf("\nScript name: %s\n", a.ScriptName))
		if a.ScriptBody != "" {
			sb.WriteString(fmt.Sprintf("Script content:\n%s\n", a.ScriptBody))
		}
	}

	if a.Module != "" {
		sb.WriteString(fmt.Sprintf("\nModule: %s\n", a.Module))
		if a.ModuleArgs != "" {
			sb.WriteString(fmt.Sprintf("Module args: %s\n", a.ModuleArgs))
		}
	}

	if a.PlaybookPath != "" {
		sb.WriteString(fmt.Sprintf("\nPlaybook path: %s\n", a.PlaybookPath))
	}

	if len(a.PlaybookFiles) > 0 {
		sb.WriteString("\nPlaybook files:\n")
		for path, content := range a.PlaybookFiles {
			sb.WriteString(fmt.Sprintf("--- %s ---\n%s\n\n", path, content))
		}
	}

	if a.ExtraArgs != "" {
		sb.WriteString(fmt.Sprintf("\nExtra args: %s\n", a.ExtraArgs))
	}

	if a.PackagePath != "" {
		sb.WriteString(fmt.Sprintf("\nDeploy artifact: %s\n", a.PackagePath))
	}
	if a.InstallCommand != "" {
		sb.WriteString(fmt.Sprintf("Install command: %s\n", a.InstallCommand))
	}
	if a.DestPath != "" {
		sb.WriteString(fmt.Sprintf("Destination path: %s\n", a.DestPath))
	}

	sb.WriteString(`
Focus on:
1. destructive operations
2. mistakes in targeting or host scope
3. likely typos
4. risky shell usage
5. privilege misuse
6. whether this should require strong confirmation
`)

	return sb.String()
}

// gatherPlaybookFiles reads a playbook and recursively resolves all
// referenced task files, roles, and handlers into a flat map.
func gatherPlaybookFiles(playbookPath string) map[string]string {
	files := map[string]string{}
	seen := map[string]bool{}
	baseDir := filepath.Dir(playbookPath)

	var walk func(path string)
	walk = func(path string) {
		absPath, err := filepath.Abs(path)
		if err != nil || seen[absPath] {
			return
		}
		seen[absPath] = true

		content, err := os.ReadFile(path)
		if err != nil {
			return
		}
		files[path] = string(content)

		// Parse YAML to find references.
		var docs []any
		dec := yaml.NewDecoder(bytes.NewReader(content))
		for {
			var doc any
			if err := dec.Decode(&doc); err != nil {
				break
			}
			docs = append(docs, doc)
		}

		// Walk the parsed YAML looking for task references and roles.
		for _, ref := range extractFileRefs(docs) {
			resolved := ref
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(path), ref)
			}
			walk(resolved)
		}

		for _, role := range extractRoleNames(docs) {
			// Check standard role paths.
			for _, roleBase := range []string{
				filepath.Join(baseDir, "roles", role),
				filepath.Join(filepath.Dir(path), "roles", role),
			} {
				for _, sub := range []string{
					"tasks/main.yml", "tasks/main.yaml",
					"handlers/main.yml", "handlers/main.yaml",
					"defaults/main.yml", "vars/main.yml",
				} {
					walk(filepath.Join(roleBase, sub))
				}
			}
		}
	}

	walk(playbookPath)
	return files
}

// extractFileRefs finds import_tasks, include_tasks, import_playbook values.
func extractFileRefs(docs []any) []string {
	var refs []string
	var walk func(v any)
	walk = func(v any) {
		switch val := v.(type) {
		case map[string]any:
			for _, key := range []string{
				"import_tasks", "include_tasks",
				"import_playbook", "include",
				"ansible.builtin.import_tasks",
				"ansible.builtin.include_tasks",
				"ansible.builtin.import_playbook",
			} {
				if ref, ok := val[key]; ok {
					if s, ok := ref.(string); ok && s != "" {
						refs = append(refs, s)
					}
				}
			}
			for _, child := range val {
				walk(child)
			}
		case []any:
			for _, item := range val {
				walk(item)
			}
		}
	}
	for _, doc := range docs {
		walk(doc)
	}
	return refs
}

// extractRoleNames finds role names from roles: lists.
func extractRoleNames(docs []any) []string {
	var roles []string
	var walk func(v any)
	walk = func(v any) {
		switch val := v.(type) {
		case map[string]any:
			if roleList, ok := val["roles"]; ok {
				if list, ok := roleList.([]any); ok {
					for _, item := range list {
						switch r := item.(type) {
						case string:
							roles = append(roles, r)
						case map[string]any:
							if name, ok := r["role"].(string); ok {
								roles = append(roles, name)
							}
							if name, ok := r["name"].(string); ok {
								roles = append(roles, name)
							}
						}
					}
				}
			}
			for _, child := range val {
				walk(child)
			}
		case []any:
			for _, item := range val {
				walk(item)
			}
		}
	}
	for _, doc := range docs {
		walk(doc)
	}
	return roles
}
