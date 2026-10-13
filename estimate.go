package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

const defaultEstimateModel = "claude-opus-5-5"

// USD per million input and output tokens, from Anthropic's list as cached on
// 2026-09-25. Only for showing what an estimate cost; the cap on spending is
// the Anthropic workspace limit on the key. A model missing here shows no cost.
var modelPrices = map[string][2]float64{
	"claude-fable-5-1":  {10, 50},
	"claude-fable-5":    {10, 50},
	"claude-opus-5-5":   {4, 20},
	"claude-opus-5":     {5, 25},
	"claude-opus-4-8":   {5, 25},
	"claude-sonnet-5-5": {2, 10},
	"claude-sonnet-5":   {2, 10},
	"claude-haiku-4-5":  {1, 5},
}

func estimateCost(model string, u estimateUsage) *float64 {
	p, ok := modelPrices[model]
	if !ok {
		return nil
	}
	c := float64(u.InputTokens)*p[0]/1e6 + float64(u.OutputTokens)*p[1]/1e6
	return &c
}

// The model is trusted to say what is on the plate and roughly how much. It
// is not trusted for nutrient values when the food database has the food, so
// it is made to look foods up and name the row it means.
const estimateSystem = `You estimate a single meal for a personal calorie log in Sweden. You get a photo of the plate, a short description, or both.

1. Work out the components and estimate the cooked/served weight in grams of each. The description wins over the photo where they disagree; it is where hidden fat (cream, butter, frying oil), portion size and second helpings are mentioned. Restaurant lunch portions in Sweden are usually generous, and sauces and dressings are easy to miss.
2. For each component, call search_foods with short Swedish terms ("ris kokt", "kycklinggryta", "bearnaisesås"). Prefer a prepared or cooked entry over a raw ingredient when the component is served cooked. Search a couple of phrasings if the first misses.
3. Finish by calling submit_estimate exactly once. Use the food_id of the best match; use 0 only when nothing found is close, and then your per-100 g values are used instead. Always fill per_100g with your own estimate either way.

Split a dish into the parts you can see (stew, rice, salad, bread, butter, sauce) rather than one line for the whole plate, unless the database has the composite dish itself. Do not include drinks unless they are visible or described.`

type estimateItem struct {
	Name   string  `json:"name"`
	FoodID int64   `json:"food_id"`
	Grams  float64 `json:"grams"`
	Per100 struct {
		Kcal    float64 `json:"kcal"`
		Protein float64 `json:"protein"`
		Carbs   float64 `json:"carbs"`
		Fat     float64 `json:"fat"`
	} `json:"per_100g"`
}

type estimateResult struct {
	Dish       string         `json:"dish"`
	Note       string         `json:"note"`
	Components []estimateItem `json:"components"`
}

type estimateUsage struct {
	InputTokens  int64 `json:"inputTokens"`
	OutputTokens int64 `json:"outputTokens"`
}

func obj(props map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

var numSchema = map[string]any{"type": "number"}

func estimateTools() []anthropic.ToolUnionParam {
	search := anthropic.ToolParam{
		Name: "search_foods",
		Description: anthropic.String("Search the food database (Livsmedelsverket plus foods saved locally). " +
			"Every word must appear in the name, so use one to three short Swedish words. Returns up to 15 foods with nutrients per 100 g."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{"query": map[string]any{"type": "string"}},
			Required:   []string{"query"},
		},
	}
	submit := anthropic.ToolParam{
		Name:        "submit_estimate",
		Description: anthropic.String("Submit the final estimate. Call exactly once, after searching."),
		Strict:      anthropic.Bool(true),
		InputSchema: anthropic.ToolInputSchemaParam{
			Properties: map[string]any{
				"dish": map[string]any{"type": "string", "description": "Short name for the meal, in Swedish"},
				"note": map[string]any{"type": "string", "description": "One short sentence on what is most uncertain, or empty"},
				"components": map[string]any{"type": "array", "items": obj(map[string]any{
					"name":     map[string]any{"type": "string"},
					"food_id":  map[string]any{"type": "integer", "description": "id from search_foods, or 0 if nothing fits"},
					"grams":    numSchema,
					"per_100g": obj(map[string]any{"kcal": numSchema, "protein": numSchema, "carbs": numSchema, "fat": numSchema}, "kcal", "protein", "carbs", "fat"),
				}, "name", "food_id", "grams", "per_100g")},
			},
			Required:    []string{"dish", "note", "components"},
			ExtraFields: map[string]any{"additionalProperties": false},
		},
	}
	return []anthropic.ToolUnionParam{{OfTool: &search}, {OfTool: &submit}}
}

type foodHit struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Brand   string  `json:"brand,omitempty"`
	Kcal    float64 `json:"kcal"`
	Protein float64 `json:"protein"`
	Carbs   float64 `json:"carbs"`
	Fat     float64 `json:"fat"`
}

// estimate runs the search → submit loop. raw is the submitted JSON, kept so
// a later parsing change can be checked against what the model actually said.
func estimate(ctx context.Context, client anthropic.Client, model string, store *Store, uid int64, photo []byte, mediaType, text string) (*estimateResult, json.RawMessage, estimateUsage, error) {
	var usage estimateUsage
	var content []anthropic.ContentBlockParamUnion
	if len(photo) > 0 {
		content = append(content, anthropic.NewImageBlockBase64(mediaType, base64.StdEncoding.EncodeToString(photo)))
	}
	prompt := "Estimate this meal."
	if text != "" {
		prompt = "Estimate this meal. My description: " + text
	}
	content = append(content, anthropic.NewTextBlock(prompt))
	messages := []anthropic.MessageParam{anthropic.NewUserMessage(content...)}

	for turn := 0; turn < 10; turn++ {
		resp, err := client.Messages.New(ctx, anthropic.MessageNewParams{
			Model:     model,
			MaxTokens: 16000,
			System:    []anthropic.TextBlockParam{{Text: estimateSystem}},
			Tools:     estimateTools(),
			Messages:  messages,
		})
		if err != nil {
			return nil, nil, usage, err
		}
		usage.InputTokens += resp.Usage.InputTokens
		usage.OutputTokens += resp.Usage.OutputTokens
		if resp.StopReason == anthropic.StopReasonRefusal {
			return nil, nil, usage, errors.New("the model declined to estimate this meal")
		}
		messages = append(messages, resp.ToParam())

		var results []anthropic.ContentBlockParamUnion
		for _, block := range resp.Content {
			use, ok := block.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			raw := json.RawMessage(use.JSON.Input.Raw())
			switch use.Name {
			case "submit_estimate":
				var r estimateResult
				if err := json.Unmarshal(raw, &r); err != nil {
					return nil, raw, usage, fmt.Errorf("estimate: %w", err)
				}
				return &r, raw, usage, nil
			case "search_foods":
				var in struct{ Query string }
				if err := json.Unmarshal(raw, &in); err != nil {
					results = append(results, anthropic.NewToolResultBlock(use.ID, "bad input: "+err.Error(), true))
					continue
				}
				results = append(results, anthropic.NewToolResultBlock(use.ID, searchForModel(store, uid, in.Query), false))
			default:
				results = append(results, anthropic.NewToolResultBlock(use.ID, "unknown tool", true))
			}
		}
		if len(results) == 0 {
			// It answered in prose instead of submitting; ask once more.
			messages = append(messages, anthropic.NewUserMessage(anthropic.NewTextBlock("Call submit_estimate with your estimate.")))
			continue
		}
		messages = append(messages, anthropic.NewUserMessage(results...))
	}
	return nil, nil, usage, errors.New("estimate: no answer after 10 turns")
}

func searchForModel(store *Store, uid int64, q string) string {
	foods, err := store.SearchFoods(uid, q, 15)
	if err != nil {
		return "search failed: " + err.Error()
	}
	if len(foods) == 0 {
		return fmt.Sprintf("no foods contain every word of %q; try fewer or other words", q)
	}
	hits := make([]foodHit, len(foods))
	for i, f := range foods {
		hits[i] = foodHit{f.ID, f.Name, f.Brand, f.Kcal, f.Protein, f.Carbs, f.Fat}
	}
	b, _ := json.Marshal(hits)
	return string(b)
}

// proposal turns the model's answer into foods the page can show: a real food
// where the named id exists, otherwise an unsaved draft from the model's own
// per-100 g values, which is only written if the meal is accepted.
func proposal(store *Store, r *estimateResult) ([]proposedItem, error) {
	out := []proposedItem{}
	for _, c := range r.Components {
		if c.Grams <= 0 {
			continue
		}
		var f *Food
		if c.FoodID > 0 {
			var err error
			if f, err = store.FoodByID(c.FoodID); err != nil {
				return nil, err
			}
		}
		if f == nil {
			f = &Food{Source: "estimate", Name: strings.TrimSpace(c.Name),
				Nutrients: Nutrients{Kcal: c.Per100.Kcal, Protein: c.Per100.Protein, Carbs: c.Per100.Carbs, Fat: c.Per100.Fat}}
		}
		out = append(out, proposedItem{Name: c.Name, Grams: c.Grams, Food: *f})
	}
	return out, nil
}

type proposedItem struct {
	Name  string  `json:"name"`
	Grams float64 `json:"grams"`
	Food  Food    `json:"food"`
}
