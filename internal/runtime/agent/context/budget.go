package agentcontext

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"

	"github.com/fwtllh-png/QCode/internal/adapter/model"
	"github.com/fwtllh-png/QCode/internal/adapter/provider"
	"github.com/fwtllh-png/QCode/internal/platform/tokenestimate"
	"github.com/fwtllh-png/QCode/internal/runtime/protocol"
)

func EstimateMessageTokens(messages []provider.Message) uint64 {
	var tokens uint64
	for _, message := range messages {
		for _, block := range message.Blocks {
			tokens += tokenestimate.Text(block.Text)
			if block.ToolCall != nil {
				tokens += tokenestimate.Text(
					block.ToolCall.Name + block.ToolCall.Arguments,
				)
			}
			if block.ToolResult != nil {
				tokens += tokenestimate.Text(block.ToolResult.Content)
			}
			if block.Type == provider.ContentImage && block.Attachment != nil {
				tokens += EstimateImageTokens(*block.Attachment)
			}
		}
	}
	return uint64(max(1, tokens))
}

// EstimateImageTokens reproduces the documented vision tokenization shared by
// provider image pricing: a base 85 tokens per image plus 170 tokens per
// 512×512 tile after the long edge is capped at 2048 and the short edge at
// 768 (Anthropic, "Vision estimates"). Payloads that cannot be decoded fall
// back to one tile per 512 KiB, an upper bound on the tile count of any
// image that encodes to that many bytes.
func EstimateImageTokens(attachment provider.Attachment) uint64 {
	config, _, err := image.DecodeConfig(bytes.NewReader(attachment.Data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		tiles := max(1, (len(attachment.Data)+(512<<10)-1)/(512<<10))
		return 85 + uint64(170*tiles)
	}
	width, height := config.Width, config.Height
	if longest := max(width, height); longest > 2048 {
		width = max(1, width*2048/longest)
		height = max(1, height*2048/longest)
	}
	if shortest := min(width, height); shortest > 768 {
		width = max(1, width*768/shortest)
		height = max(1, height*768/shortest)
	}
	tiles := ((width + 511) / 512) * ((height + 511) / 512)
	return 85 + uint64(170*tiles)
}

type BudgetRequest struct {
	ContextTokens  uint64
	EstimatedInput uint64
	OutputReserve  uint64
	SessionUsage   provider.Usage
	TurnUsage      provider.Usage
	StepUsage      provider.Usage
	MaxTokens      uint64
	SpentCostUSD   float64
	MaxCostUSD     float64
	Pricing        model.Pricing
	Scope          string
}

type Capacity struct {
	ContextTokens   uint64
	OutputCeiling   uint64
	HardInputTokens uint64
	LimitSource     model.Provenance
	OutputSource    string
}

func ResolveCapacity(
	route model.ReadyRoute,
	configuredOutput uint64,
	maxTurnTokens uint64,
	maxSessionTokens uint64,
) Capacity {
	descriptor := route.Model()
	output := descriptor.Limits.MaxOutputTokens
	source := "model_capability"
	if configuredOutput != 0 {
		output = min(output, configuredOutput)
		source = "operator_config"
	}
	for _, limit := range []uint64{maxTurnTokens, maxSessionTokens} {
		if limit != 0 && limit < output {
			output = limit
			source = "operator_token_budget"
		}
	}
	return Capacity{
		ContextTokens: descriptor.Limits.ContextTokens,
		OutputCeiling: output,
		HardInputTokens: descriptor.Limits.ContextTokens -
			min(descriptor.Limits.ContextTokens, output),
		LimitSource:  descriptor.MetadataProvenance.Limits,
		OutputSource: source,
	}
}

func ApplyCapacity(context *protocol.SampleContextData, capacity Capacity) {
	if context == nil {
		return
	}
	context.WindowHardInputTokens = capacity.HardInputTokens
	context.WindowOutputSource = capacity.OutputSource
}

func CheckBudget(request BudgetRequest) (uint64, error) {
	if request.EstimatedInput >= request.ContextTokens {
		return 0, protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"context window has no remaining output capacity",
			true,
			nil,
		)
	}
	outputReserve := min(
		request.OutputReserve,
		request.ContextTokens-request.EstimatedInput,
	)
	usedTokens := request.SessionUsage.Total() +
		request.TurnUsage.Total() +
		request.StepUsage.Total() +
		request.EstimatedInput
	if request.MaxTokens > 0 {
		if usedTokens >= request.MaxTokens {
			return 0, protocol.NewBudgetExhausted(
				protocol.BudgetExhaustion{
					Resource: protocol.BudgetResourceTokens,
					Scope:    request.Scope, Used: usedTokens,
					Limit: request.MaxTokens, Projected: true,
				},
				nil,
			)
		}
		outputReserve = min(outputReserve, request.MaxTokens-usedTokens)
	}
	if request.MaxCostUSD > 0 {
		if !request.Pricing.Known {
			return 0, protocol.NewProblem(
				protocol.CodeInvalidArgument,
				"cost budget requires known model pricing",
				false,
				nil,
			)
		}
		projectedUsage := request.TurnUsage
		projectedUsage.Add(request.StepUsage)
		projectedUsage.Add(provider.Usage{
			InputTokens: request.EstimatedInput,
		})
		spent := request.SpentCostUSD +
			provider.EstimateCost(request.Pricing, projectedUsage)
		if spent >= request.MaxCostUSD {
			return 0, costBudgetExhausted(request, spent)
		}
		if request.Pricing.OutputPerMillion > 0 {
			affordable := uint64(math.Floor(
				(request.MaxCostUSD - spent) * 1_000_000 /
					request.Pricing.OutputPerMillion,
			))
			outputReserve = min(outputReserve, affordable)
			if outputReserve == 0 {
				return 0, costBudgetExhausted(request, spent)
			}
		}
	}
	if outputReserve == 0 {
		return 0, protocol.NewProblem(
			protocol.CodeResourceExhausted,
			"budget has no remaining output capacity",
			false,
			nil,
		)
	}
	return outputReserve, nil
}

func costBudgetExhausted(request BudgetRequest, spent float64) error {
	return protocol.NewBudgetExhausted(
		protocol.BudgetExhaustion{
			Resource: protocol.BudgetResourceCostMicrounits,
			Scope:    request.Scope,
			Used:     uint64(math.Ceil(spent * 1e6)),
			Limit: max(
				uint64(1),
				uint64(math.Ceil(request.MaxCostUSD*1e6)),
			),
			Projected: true,
		},
		nil,
	)
}
