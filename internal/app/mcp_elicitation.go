package app

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"lingzhu/internal/agent"
	"lingzhu/internal/question"
)

type elicitationParams struct {
	Message         string `json:"message"`
	Mode            string `json:"mode"`
	URL             string `json:"url"`
	ElicitationID   string `json:"elicitationId"`
	RequestedSchema struct {
		Type       string                     `json:"type"`
		Required   []string                   `json:"required"`
		Properties map[string]elicitationProp `json:"properties"`
	} `json:"requestedSchema"`
}

type elicitationProp struct {
	Type        string            `json:"type"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Enum        []any             `json:"enum"`
	Items       *elicitationProp  `json:"items"`
	EnumNames   []string          `json:"enumNames"`
	OneOf       []elicitationProp `json:"oneOf"`
	Const       any               `json:"const"`
	Default     any               `json:"default"`
}

func elicitationHandler(runner *agent.Agent) func(ctx context.Context, server, sessionID string, params json.RawMessage) (any, error) {
	return func(ctx context.Context, _ string, sessionID string, params json.RawMessage) (any, error) {
		var request elicitationParams
		if err := json.Unmarshal(params, &request); err != nil {
			return map[string]any{"action": "decline"}, nil
		}
		// URL elicitation needs a browser handoff. Show the URL in a question so
		// the user can complete it manually, then retry through the normal flow.
		if strings.EqualFold(strings.TrimSpace(request.Mode), "url") {
			url := strings.TrimSpace(request.URL)
			if url == "" {
				return map[string]any{"action": "decline"}, nil
			}
			message := strings.TrimSpace(request.Message)
			if message == "" {
				message = "Complete the authorization and continue."
			}
			response, err := runner.Ask(ctx, sessionID, []question.Prompt{{
				Header:   "Authorization",
				Question: message + "\n" + url,
				Options: []question.Option{
					{Label: "Done", Description: "I completed the authorization."},
					{Label: "Cancel", Description: "Cancel the request."},
				},
			}})
			if err != nil {
				return map[string]any{"action": "decline"}, nil
			}
			if response.Cancelled || len(response.Answers) == 0 || len(response.Answers[0]) == 0 {
				return map[string]any{"action": "cancel"}, nil
			}
			if strings.EqualFold(strings.TrimSpace(response.Answers[0][0]), "Cancel") {
				return map[string]any{"action": "cancel"}, nil
			}
			return map[string]any{"action": "accept"}, nil
		}
		keys := make([]string, 0, len(request.RequestedSchema.Properties))
		for key := range request.RequestedSchema.Properties {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		prompts := make([]question.Prompt, 0, len(keys))
		for _, key := range keys {
			property := request.RequestedSchema.Properties[key]
			label := strings.TrimSpace(property.Title)
			if label == "" {
				label = key
			}
			if len(label) > 30 {
				label = label[:30]
			}
			text := strings.TrimSpace(property.Description)
			if text == "" {
				text = label
			}
			prompt := question.Prompt{
				Header:   label,
				Question: text,
				Options:  elicitationOptions(property),
			}
			if property.Type == "array" {
				prompt.Multiple = true
			}
			prompts = append(prompts, prompt)
		}
		if len(prompts) == 0 {
			prompts = append(prompts, question.Prompt{
				Header:   "Input",
				Question: request.Message,
				Options:  []question.Option{{Label: "Continue", Description: "Accept the request."}},
			})
		}
		response, err := runner.Ask(ctx, sessionID, prompts)
		if err != nil {
			return map[string]any{"action": "decline"}, nil
		}
		if response.Cancelled {
			return map[string]any{"action": "cancel"}, nil
		}
		content := map[string]any{}
		for index, key := range keys {
			property := request.RequestedSchema.Properties[key]
			var answers []string
			if index < len(response.Answers) {
				answers = response.Answers[index]
			}
			if len(answers) == 0 {
				continue
			}
			content[key] = elicitationValue(property, answers)
		}
		return map[string]any{"action": "accept", "content": content}, nil
	}
}

func elicitationOptions(property elicitationProp) []question.Option {
	var options []question.Option
	if len(property.OneOf) > 0 {
		for _, item := range property.OneOf {
			label := strings.TrimSpace(item.Title)
			if label == "" {
				label = fmt.Sprint(item.Const)
			}
			options = append(options, question.Option{Label: label, Description: item.Description})
		}
	}
	if len(options) == 0 && len(property.Enum) > 0 {
		for index, value := range property.Enum {
			label := fmt.Sprint(value)
			if index < len(property.EnumNames) && property.EnumNames[index] != "" {
				label = property.EnumNames[index]
			}
			options = append(options, question.Option{Label: label, Description: label})
		}
	}
	if property.Type == "boolean" {
		options = []question.Option{
			{Label: "true", Description: "Accept"},
			{Label: "false", Description: "Decline"},
		}
	}
	if property.Type == "array" && property.Items != nil {
		if len(property.Items.Enum) > 0 {
			for _, value := range property.Items.Enum {
				label := fmt.Sprint(value)
				options = append(options, question.Option{Label: label, Description: label})
			}
		}
	}
	if len(options) == 0 {
		options = []question.Option{{Label: "Enter a value", Description: "Type the value below."}}
	}
	return options
}

func elicitationValue(property elicitationProp, answers []string) any {
	if property.Type == "array" {
		values := make([]any, 0, len(answers))
		for _, answer := range answers {
			values = append(values, elicitationScalar(property.Items, answer))
		}
		return values
	}
	return elicitationScalar(&property, answers[0])
}

func elicitationScalar(property *elicitationProp, answer string) any {
	value := strings.TrimSpace(answer)
	if property == nil {
		return value
	}
	if len(property.OneOf) > 0 {
		for _, item := range property.OneOf {
			if strings.TrimSpace(item.Title) == value || fmt.Sprint(item.Const) == value {
				return item.Const
			}
		}
	}
	if len(property.Enum) > 0 {
		for index, item := range property.Enum {
			if fmt.Sprint(item) == value {
				return item
			}
			if index < len(property.EnumNames) && strings.TrimSpace(property.EnumNames[index]) == value {
				return item
			}
		}
	}
	switch property.Type {
	case "boolean":
		switch strings.ToLower(value) {
		case "true", "yes", "on", "1":
			return true
		default:
			return false
		}
	case "number":
		if parsed, err := strconv.ParseFloat(value, 64); err == nil {
			return parsed
		}
	case "integer":
		if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
			return parsed
		}
	}
	return value
}
