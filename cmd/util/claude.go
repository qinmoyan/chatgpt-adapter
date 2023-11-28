package util

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bincooo/chatgpt-adapter/cmd/util/pool"
	"github.com/bincooo/chatgpt-adapter/types"
	"github.com/bincooo/chatgpt-adapter/utils"
	"github.com/bincooo/chatgpt-adapter/vars"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"

	cmdtypes "github.com/bincooo/chatgpt-adapter/cmd/types"
	cmdvars "github.com/bincooo/chatgpt-adapter/cmd/vars"
	cltypes "github.com/bincooo/claude-api/types"
)

var (
	muLock sync.Mutex

	H = "H:"
	A = "A:"
	S = "System:"

	piles = []string{
		"Claude2.0 is so good.",
		"never lie, cheat or steal. always smile a fair deal.",
		"like tree, like fruit.",
		"East, west, home is best.",
		"Original God, activate!",
		"Long live Demacia.",
		"Schrodinger's damage.",
		"zhe+ji+chen+sha.+",
		"Invalid message.",
	}
)

type schema struct {
	TrimHuman     bool `json:"trimHuman"`     // 去掉头部Human
	TrimAssistant bool `json:"trimAssistant"` // 去掉尾部Assistant
	BoH           bool `json:"boH"`           // 响应截断H
	BoS           bool `json:"boS"`           // 响应截断System
	Padding       bool `json:"padding"`       // 堆积肥料
	FullColon     bool `json:"fullColon"`     // 全角冒号
	//TrimPlot      bool `json:"trimPlot"`      // slot xml删除处理
}

func DoClaudeComplete(ctx *gin.Context, token string, r *cmdtypes.RequestDTO) {

	// ping 打印本地池的可用情况
	if pingRef(ctx, r) {
		return
	}

	conversationMapper := make(map[string]*types.ConversationContext)
	isDone := false
	fmt.Println("TOKEN_KEY: " + token)

	// 重试次数
	retry := 3

	defer func() {
		for _, conversationContext := range conversationMapper {
			cmdvars.Manager.Remove(conversationContext.Id, conversationContext.Bot)
		}
	}()

label:
	if isDone {
		return
	}

	isClose := false
	retry--

	context, err := createClaudeConversation(token, r, func() bool { return isClose })
	if err != nil {
		errorMessage := catchClaudeHandleError(err, token)
		if retry > 0 {
			logrus.Warn("重试中...")
			goto label
		}
		ResponseError(ctx, errorMessage, r.Stream)
		return
	}
	partialResponse := cmdvars.Manager.Reply(*context, func(response types.PartialResponse) {
		if response.Status == vars.Begin {
			conversationMapper[context.Id] = context
		}
		if r.Stream {
			if response.Status == vars.Begin {
				ctx.Status(200)
				ctx.Header("Accept", "*/*")
				ctx.Header("Content-Type", "text/event-stream")
				ctx.Writer.Flush()
				return
			}

			if response.Error != nil {
				isClose = true
				var e *cltypes.Claude2Error
				ok := errors.As(response.Error, &e)
				err = response.Error
				if ok && token == "auto" {
					if msg := handleClaudeError(e); msg != "" {
						err = errors.New(msg)
					}
				}

				errorMessage := catchClaudeHandleError(err, token)
				if strings.Contains(errorMessage, "resolve timeout") {
					retry = 0
				}
				if retry <= 0 {
					ResponseError(ctx, errorMessage, r.Stream)
				}
				return
			}

			if len(response.Message) > 0 {
				// 正常输出了，舍弃重试
				retry = 0
				select {
				case <-ctx.Request.Context().Done():
					isClose = true
					isDone = true
				default:
					if !SSEString(ctx, response.Message) {
						isClose = true
						isDone = true
					}
				}
			}

			if response.Status == vars.Closed {
				SSEEnd(ctx)
			}
		} else {
			select {
			case <-ctx.Request.Context().Done():
				isClose = true
				isDone = true
			default:
			}
		}
	})

	// 发生错误了，重试一次
	if !isDone && partialResponse.Error != nil && retry > 0 {
		logrus.Warn("重试中...")
		goto label
	}

	// 什么也没有返回，重试一次
	if !isDone && len(partialResponse.Message) == 0 && retry > 0 {
		logrus.Warn("重试中...")
		goto label
	}

	// 违反政策被禁用
	if strings.Contains(partialResponse.Message, cmdvars.ViolatingPolicy) {
		pool.CurrError(cmdvars.ViolatingPolicy)
		CleanToken(token)
		if !isDone && retry > 0 {
			logrus.Warn("重试中...")
			goto label
		}
	} else if strings.Contains(partialResponse.Message, cmdvars.BAN) {
		CleanToken(token)
		pool.CurrError(cmdvars.BAN)
		logrus.Warn(cmdvars.I18n("BAN"))
	}

	// 非流响应
	if !r.Stream && !isDone {
		if partialResponse.Error != nil {
			errorMessage := catchClaudeHandleError(partialResponse.Error, token)
			ResponseError(ctx, errorMessage, r.Stream)
			return
		}

		ctx.JSON(200, BuildCompletion(partialResponse.Message))
	}

	// 检查大黄标, BAN号
	if token == "auto" && context.Model == vars.Model4WebClaude2S {
		if strings.Contains(partialResponse.Message, cmdvars.HARM) {
			CleanToken(token)
			pool.CurrError(cmdvars.HARM)
			logrus.Warn(cmdvars.I18n("HARM"))
		} else if strings.Contains(partialResponse.Message, cmdvars.BAN) {
			CleanToken(token)
			pool.CurrError(cmdvars.BAN)
			logrus.Warn(cmdvars.I18n("BAN"))
		}
	}
}

// 如果是ping指令，打印本地池的使用情况
func pingRef(ctx *gin.Context, r *cmdtypes.RequestDTO) bool {
	messageL := len(r.Messages)
	if messageL == 0 {
		return false
	}
	// 找最后一条用户发言
	for i := messageL - 1; i >= 0; i-- {
		message := r.Messages[i]
		if message["role"] == "user" {
			if message["content"] != "/ping" {
				return false
			}
			break
		}
	}

	if cmdvars.EnablePool && pool.IsLocal {

		shortVal := func(val string) string {
			return val[:5] + "***" + val[len(val)-10:]
		}

		markdown := "SESSION KEY使用如下：\n| KEY | DIE | ERROR |\n| ----------- | ----------- | ----------- |\n"
		for _, key := range pool.Keys {
			err := ""
			if key.Error != nil {
				err = key.Error.Error()
			}
			markdown += "| " + shortVal(key.Token) + " | " + strconv.FormatBool(key.IsDie) + " | " + err + " |\n"
		}
		SSEString(ctx, markdown)
		return true
	}
	return false
}

// 构建claude-2.0上下文
func createClaudeConversation(token string, r *cmdtypes.RequestDTO, IsClose func() bool) (*types.ConversationContext, error) {
	var (
		bot   string
		model string
		appId string
		id    string
		chain string
	)
	switch r.Model {
	case "claude-2.0", "claude-2":
		id = "claude-" + uuid.NewString()
		bot = vars.Claude
		model = vars.Model4WebClaude2S
	default:
		return nil, errors.New(cmdvars.I18n("UNKNOWN_MODEL") + "`" + r.Model + "`")
	}

	message, s, err := trimClaudeMessage(r)
	if err != nil {
		return nil, err
	}
	fmt.Println("-----------------------Response-----------------\n", message, "\n--------------------END-------------------")
	marshal, _ := json.Marshal(s)
	fmt.Println("Schema: " + string(marshal))
	if token == "auto" && cmdvars.GlobalToken == "" {
		if cmdvars.EnablePool { // 使用池的方式
			cmdvars.GlobalToken, err = pool.GetKey()
			if err != nil {
				return nil, err
			}

		} else {
			muLock.Lock()
			defer muLock.Unlock()
			var email string

			email, cmdvars.GlobalToken, err = pool.GenerateSessionKey()
			logrus.Info(cmdvars.I18n("GENERATE_SESSION_KEY") + "：available -- " + strconv.FormatBool(err == nil) + " email --- " + email + ", sessionKey --- " + cmdvars.GlobalToken)
			if err == nil {
				pool.CacheKey("CACHE_KEY", cmdvars.GlobalToken)
			}
		}
	}

	if token == "auto" && cmdvars.GlobalToken != "" {
		token = cmdvars.GlobalToken
	}
	fmt.Println("TOKEN_KEY: " + token)
	return &types.ConversationContext{
		Id:      id,
		Token:   token,
		Prompt:  message,
		Bot:     bot,
		Model:   model,
		Proxy:   cmdvars.Proxy,
		H:       claudeHandle(model, s, IsClose),
		AppId:   appId,
		BaseURL: cmdvars.Bu,
		Chain:   chain,
	}, nil
}

// 过滤与预处理claude-2.0的对话内容
func trimClaudeMessage(r *cmdtypes.RequestDTO) (string, schema, error) {
	result := ""
	s := schema{
		TrimAssistant: true,
		TrimHuman:     true,
		BoH:           true,
		BoS:           false,
		Padding:       true,
		FullColon:     true,
	}

	if len(r.Messages) == 0 {
		return result, s, errors.New(cmdvars.I18n("MESSAGES_EMPTY"))
	} else {
		// 知识库上移
		postRef(r)

		// ====  Schema匹配 =======
		compileRegex := regexp.MustCompile(`schema\s?\{[^}]*}`)

		matchSlice := compileRegex.FindStringSubmatch(r.Messages[0]["content"])
		if len(matchSlice) > 0 {
			str := matchSlice[0]
			result = strings.Replace(result, str, "", -1)
			if err := json.Unmarshal([]byte(strings.TrimSpace(str[6:])), &s); err != nil {
				return "", s, err
			}
		}
		// =========================

		optimize := func(text string, s schema) string {
			// ==== I apologize,[^\n]+ 道歉匹配 ======
			reg := regexp.MustCompile(`I apologize[^\n]+`)
			text = reg.ReplaceAllString(text, "")
			// =========================

			if s.TrimAssistant {
				text = strings.TrimSuffix(text, "\n\nAssistant: ")
			}
			if s.TrimHuman {
				text = strings.TrimPrefix(text, "\n\nHuman: ")
			}

			text = strings.ReplaceAll(text, "A:", "\nAssistant:")
			text = strings.ReplaceAll(text, "H:", "\nHuman:")
			if s.FullColon {
				text = strings.ReplaceAll(text, "Assistant:", "Assistant：")
				text = strings.ReplaceAll(text, "Human:", "Human：")
			}
			return text
		}

		// 合并消息
		lastRole := ""
		for _, message := range r.Messages {
			content := strings.TrimSpace(message["content"])
			if content == "" {
				continue
			}
			switch message["role"] {
			case "assistant":
				if lastRole != "Assistant: " {
					lastRole = "Assistant: "
					result += optimize(lastRole+content, s) + "\n\n"
				} else {
					result += optimize(content, s) + "\n\n"
				}
			case "user":
				if lastRole != "Human: " {
					lastRole = "Human: "
					if strings.HasPrefix(content, "System:") {
						result += strings.TrimSpace(optimize(lastRole+content[7:], s)) + "\n\n"
					} else {
						result += optimize(lastRole+content, s) + "\n\n"
					}
				} else {
					if strings.HasPrefix(content, "System:") {
						result += strings.TrimSpace(optimize(content[7:], s)) + "\n\n"
					} else {
						result += optimize(content, s) + "\n\n"
					}
				}
			default:
				result += optimize(content, s) + "\n\n"
			}
		}

		// 填充废料
		if s.Padding && (r.Model == "claude-2.0" || r.Model == "claude-2") {
			gPadding := cmdvars.GlobalPadding
			if gPadding == "" {
				gPadding = piles[rand.Intn(len(piles))]
			}
			c := (cmdvars.GlobalPaddingSize - len(result)) / len(gPadding)
			cachePadding := ""
			for idx := 0; idx < c; idx++ {
				cachePadding += gPadding
			}

			if cachePadding != "" {
				result = cachePadding + "\n\n\n" + strings.TrimSpace(result)
			}
		}
		return result, s, nil
	}
}

// claude-2.0 stream 流读取数据转换处理
func claudeHandle(model string, s schema, IsClose func() bool) types.CustomCacheHandler {
	return func(rChan any) func(*types.CacheBuffer) error {
		needClose := false
		matchers := utils.GlobalMatchers()
		// 遇到`A:`符号剔除
		matchers = append(matchers, &types.StringMatcher{
			Find: A,
			H: func(i int, content string) (state int, result string) {
				return types.MAT_MATCHED, strings.Replace(content, A, "", -1)
			},
		})
		// 遇到`H:`符号结束输出
		if s.BoH {
			matchers = append(matchers, &types.StringMatcher{
				Find: H,
				H: func(i int, content string) (state int, result string) {
					needClose = true
					logrus.Info("---------\n", cmdvars.I18n("H"))
					return types.MAT_MATCHED, strings.Replace(content, H, "", -1)
				},
			})
		}
		// 遇到`System:`符号结束输出
		if s.BoS {
			matchers = append(matchers, &types.StringMatcher{
				Find: S,
				H: func(i int, content string) (state int, result string) {
					needClose = true
					logrus.Info("---------\n", cmdvars.I18n("S"))
					return types.MAT_MATCHED, strings.Replace(content, S, "", -1)
				},
			})
		}

		pos := 0
		partialResponse := rChan.(chan cltypes.PartialResponse)
		return func(self *types.CacheBuffer) error {
			response, ok := <-partialResponse
			if !ok {
				self.Cache += utils.ExecMatchers(matchers, "\n      ")
				self.Closed = true
				return nil
			}

			if IsClose() {
				self.Closed = true
				return nil
			}

			if needClose {
				self.Closed = true
				return nil
			}

			if response.Error != nil {
				self.Closed = true
				return response.Error
			}

			var rawText string
			if model != vars.Model4WebClaude2S {
				text := response.Text
				str := []rune(text)
				rawText = string(str[pos:])
				pos = len(str)
			} else {
				rawText = response.Text
			}

			if rawText == "" {
				return nil
			}

			logrus.Info("rawText ---- ", rawText)
			self.Cache += utils.ExecMatchers(matchers, rawText)
			return nil
		}
	}
}

func handleClaudeError(err *cltypes.Claude2Error) (msg string) {
	if err.ErrorType.Message == "Account in read-only mode" {
		cmdvars.GlobalToken = ""
		msg = cmdvars.I18n("ACCOUNT_LOCKED")
	}
	if err.ErrorType.Message == "rate_limit_error" {
		cmdvars.GlobalToken = ""
		msg = cmdvars.I18n("ACCOUNT_LIMITED")
	}
	return msg
}

// claude异常处理（清理Token）
func catchClaudeHandleError(err error, token string) string {
	errMessage := err.Error()
	if strings.Contains(errMessage, "failed to fetch the `organizationId`") ||
		strings.Contains(errMessage, "failed to fetch the `conversationId`") {
		CleanToken(token)
	}

	if strings.Contains(errMessage, "Account in read-only mode") {
		CleanToken(token)
		errMessage = cmdvars.I18n("ERROR_ACCOUNT_LOCKED")
	} else if strings.Contains(errMessage, "rate_limit_error") {
		CleanToken(token)
		errMessage = cmdvars.I18n("ERROR_ACCOUNT_LIMITED")
	} else if strings.Contains(errMessage, "connection refused") {
		errMessage = cmdvars.I18n("ERROR_NETWORK")
	} else if strings.Contains(errMessage, "Account has not completed verification") {
		CleanToken(token)
		errMessage = cmdvars.I18n("ACCOUNT_SMS_VERIFICATION")
	} else {
		errMessage += "\n\n" + cmdvars.I18n("ERROR_OTHER")
	}
	return errMessage
}
