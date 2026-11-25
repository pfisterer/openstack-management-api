package webserver

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/apierr"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/openstack-management-api/internal/assistant"
	"github.com/pfisterer/openstack-management-api/internal/identity"
)

// The portal assistant (package assistant, docs/portal-assistant.md), stage 1 (decided 25.11.2026: there is no stage that executes; a write is only ever proposed as a prefilled dialog): questions in the person's own words, answered by the platform's language model from what this service's own MCP tools return. The tools run in this process over an in-memory MCP session, built for the caller as /mcp builds them — same rights, same payloads — but for a READ-ONLY caller, so no tool that writes exists in that session, and the assistant offers only assistant.ReadTools on top. Nothing is stored.

// AssistantConfig wires the assistant; nil answers its route with 503.
type AssistantConfig struct {
	Agent *assistant.Agent
	// Limiter bounds questions per person and at once; nil for none.
	Limiter *assistant.Limiter
}

var assistantErrors = apierr.Classifier{
	Rules: append([]apierr.Rule{
		apierr.IsWithText(assistant.ErrInvalid, http.StatusBadRequest, "invalid"),
		apierr.Is(assistant.ErrModelUnavailable, http.StatusBadGateway, "model_unavailable", msg.T("Das Sprachmodell antwortet gerade nicht. Bitte später noch einmal versuchen.", "The language model does not answer right now. Please try again later.")),
	}, serviceErrors.Rules...),
	Fallback: serviceErrors.Fallback,
}

var (
	errAssistantOff  = apierr.New(http.StatusServiceUnavailable, "assistant_unavailable", msg.T("Der Assistent ist hier nicht eingerichtet.", "The assistant is not set up here."))
	errAssistantBusy = apierr.New(http.StatusTooManyRequests, "assistant_busy", msg.T("Der Assistent ist gerade ausgelastet. Bitte gleich noch einmal versuchen.", "The assistant is busy right now. Please try again in a moment."))
	errAssistantMany = apierr.New(http.StatusTooManyRequests, "assistant_quota", msg.T("Für diese Stunde sind alle Fragen an den Assistenten verbraucht. Bitte später weiterfragen.", "All questions to the assistant for this hour are used up. Please continue later."))
)

// postAssistantAsk answers a question.
//
//	@Summary		Ask the portal assistant (read only)
//	@Description	The platform's language model answers a question about the caller's projects, budgets, allocations and requests, using only the read tools of /mcp (list_my_projects, get_project, list_requests_to_decide, …) as the caller and with the caller's rights; nothing can be changed through it. The loop is bounded in steps, tool calls, tokens and time; stopped names the bound that ended it early. tool_calls lists every call made with its arguments and result, so the portal can show what the answer rests on. history carries earlier turns of the conversation (the service keeps none). A read-only API token may ask: the tools are read-only whatever the token. 503 while no model is configured, 429 when the person's questions per hour are used up or too many run at once, 502 when the model does not answer.
//	@Tags			assistant
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		assistant.Question	true	"Question"
//	@Success		200		{object}	assistant.Answer
//	@Failure		400		{object}	map[string]any	"Empty or too long."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		429		{object}	map[string]any	"Too many questions."
//	@Failure		502		{object}	map[string]any	"The model did not answer."
//	@Failure		503		{object}	map[string]any	"Not configured."
//	@ID				postAssistantAsk
//	@Router			/v1/assistant/ask [post]
func postAssistantAsk(cfg APIConfig) gin.HandlerFunc {
	read := jsonBody[assistant.Question](msg.Text{})
	return func(c *gin.Context) {
		if cfg.Assistant == nil || cfg.Assistant.Agent == nil {
			ginweb.AbortError(c, errAssistantOff)
			return
		}
		var q assistant.Question
		if !read(c, &q) {
			return
		}
		auth, ok := caller(c)
		if !ok {
			return
		}
		if l := cfg.Assistant.Limiter; l != nil {
			// Counted for the real caller: an admin acting for somebody spends their own questions.
			release, reason, retry := l.Acquire(auth.ActorEmail)
			if release == nil {
				c.Header("Retry-After", strconv.Itoa(int(math.Ceil(retry.Seconds()))))
				if reason == "quota" {
					ginweb.AbortError(c, errAssistantMany)
				} else {
					ginweb.AbortError(c, errAssistantBusy)
				}
				return
			}
			defer release()
		}
		lang := msg.FromRequest(c.Request)
		start := time.Now()
		ans, err := cfg.askAssistant(c.Request.Context(), auth, q, lang)
		if err != nil {
			e := assistantErrors.Classify(err)
			if errors.Is(err, assistant.ErrModelUnavailable) && cfg.Log != nil {
				cfg.Log.Warnw("Assistant: the model did not answer", "actor", auth.ActorEmail, "error", err)
			}
			cfg.respond(c, e)
			return
		}
		if cfg.Log != nil {
			// Counts, not content: neither the question nor the answer is logged or stored.
			names := make([]string, 0, len(ans.ToolCalls))
			for _, tc := range ans.ToolCalls {
				names = append(names, tc.Name)
			}
			cfg.Log.Infow("Assistant: question answered", "actor", auth.ActorEmail, "user", auth.UserEmail, "steps", ans.Usage.Steps,
				"prompt_tokens", ans.Usage.PromptTokens, "completion_tokens", ans.Usage.CompletionTokens, "tools", names, "stopped", ans.Stopped, "ms", time.Since(start).Milliseconds())
		}
		c.JSON(http.StatusOK, ans)
	}
}

// askAssistant runs the agent with the read tools of an MCP server built for the caller, read only.
func (cfg APIConfig) askAssistant(ctx context.Context, auth identity.AuthContext, q assistant.Question, lang msg.Lang) (assistant.Answer, error) {
	who := mcpCaller{actorEmail: auth.ActorEmail, userEmail: auth.UserEmail, tokens: auth.EffectiveTokens, readOnly: true}
	box, err := assistant.NewMCPToolbox(msg.WithLang(ctx, lang), newMCPServer(cfg, who, cfg.Log), assistant.ReadTools)
	if err != nil {
		return assistant.Answer{}, err
	}
	defer func() { _ = box.Close() }()
	return cfg.Assistant.Agent.Ask(msg.WithLang(ctx, lang), q, box, lang)
}
