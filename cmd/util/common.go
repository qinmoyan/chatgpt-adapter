package util

import (
	"encoding/json"
	"fmt"
	cmdtypes "github.com/bincooo/chatgpt-adapter/cmd/types"
	"github.com/bincooo/chatgpt-adapter/cmd/vars"
	"github.com/bincooo/requests"
	"github.com/bincooo/requests/url"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type encoded string

func (e encoded) MarshalJSON() ([]byte, error) {
	return []byte(strconv.QuoteToASCII(string(e))), nil
}

func CleanToken(token string) {
	if token == "auto" {
		vars.GlobalToken = ""
	}
}

// 将知识库的内容往上挪
func postRef(r *cmdtypes.RequestDTO) {
	if messageL := len(r.Messages); messageL > 2 {
		pos := 2 // 1次
		// 最多上挪3次对话
		if messageL > 4 {
			pos = 4 // 2次
		}
		if messageL > 6 {
			pos = 6 // 3次
		}

		var slice []string
		content := r.Messages[messageL-1]["content"]
		for {
			start := strings.Index(content, "<postRef>")
			end := strings.Index(content, "</postRef>")
			if start < 0 {
				break
			}
			if start < end {
				slice = append(slice, content[start+9:end])
				content = content[:start] + content[end+10:]
			}
		}
		r.Messages[messageL-1]["content"] = content

		if len(slice) > 0 {
			prefix := "System: "
			if r.Model != "claude-2.0" {
				prefix = ""
			}
			r.Messages = append(r.Messages[:messageL-pos], append([]map[string]string{
				{
					"role":    "user",
					"content": prefix + strings.Join(slice, "\n\n"),
				},
			}, r.Messages[messageL-pos:]...)...)
		}
	}
}

func ResponseError(ctx *gin.Context, err string, isStream bool) {
	logrus.Error(err)
	if isStream {
		if ctx.Writer.Header().Get("Content-Type") == "" {
			ctx.Writer.Header().Set("Content-Type", "text/event-stream")
		}
		marshal, e := json.Marshal(BuildCompletion("Error: " + err))
		if e != nil {
			return
		}
		ctx.String(200, "data: %s\n\ndata: [DONE]", string(marshal))
	} else {
		ctx.JSON(200, BuildCompletion("Error: "+err))
	}
}

func SSEString(ctx *gin.Context, content string) bool {
	if ctx.Writer.Header().Get("Content-Type") == "" {
		ctx.Writer.Header().Set("Content-Type", "text/event-stream")
	}
	completion := BuildCompletion(content)
	marshal, err := json.Marshal(completion)
	if err != nil {
		logrus.Error(err)
		return false
	}
	if _, err = ctx.Writer.Write([]byte("data: " + string(marshal) + "\n\n")); err != nil {
		logrus.Error(err)
		return false
	} else {
		ctx.Writer.Flush()
		return true
	}
}

func SSEEnd(ctx *gin.Context) {
	if ctx.Writer.Header().Get("Content-Type") == "" {
		ctx.Writer.Header().Set("Content-Type", "text/event-stream")
	}
	// 结尾img标签会被吞？？多加几个空格试试
	marshal, _ := json.Marshal(BuildCompletion("  "))
	if _, err := ctx.Writer.Write(append([]byte("data: "), marshal...)); err != nil {
		logrus.Error(err)
	}
	if _, err := ctx.Writer.Write([]byte("\n\ndata: [DONE]")); err != nil {
		logrus.Error(err)
	}
}

func BuildCompletion(message string) gin.H {
	var completion gin.H
	content := gin.H{"content": message, "role": "assistant"}
	completion = gin.H{
		"choices": []gin.H{
			{
				"message": content,
				"delta":   content,
			},
		},
	}
	return completion
}

func Remove[T comparable](slice []T, t T) []T {
	return RemoveFor(slice, func(item T) bool {
		return item == t
	})
}

// 自定义条件删除元素
func RemoveFor[T comparable](slice []T, condition func(item T) bool) []T {
	if len(slice) == 0 {
		return slice
	}

	for idx, item := range slice {
		if condition(item) {
			slice = append(slice[:idx], slice[idx+1:]...)
			break
		}
	}
	return slice
}

// 判断切片是否包含子元素
func Contains[T comparable](slice []T, t T) bool {
	return ContainFor(slice, func(item T) bool {
		return item == t
	})
}

// 判断切片是否包含子元素， condition：自定义判断规则
func ContainFor[T comparable](slice []T, condition func(item T) bool) bool {
	if len(slice) == 0 {
		return false
	}

	for idx := 0; idx < len(slice); idx++ {
		if condition(slice[idx]) {
			return true
		}
	}
	return false
}

func TestNetwork(proxy string) {
	req := url.NewRequest()
	req.Timeout = 5 * time.Second
	req.Proxies = proxy
	req.AllowRedirects = false
	response, err := requests.Get("https://claude.ai/login", req)
	if err == nil && response.StatusCode == 200 {
		fmt.Println("🎉🎉🎉 Network success! 🎉🎉🎉")
		req = url.NewRequest()
		req.Timeout = 5 * time.Second
		req.Proxies = proxy
		req.Headers = url.NewHeaders()
		response, err = requests.Get("https://iphw.in0.cc/ip.php", req)
		if err == nil {
			compileRegex := regexp.MustCompile(`\d+\.\d+\.\d+\.\d+`)
			ip := compileRegex.FindStringSubmatch(response.Text)
			if len(ip) > 0 {
				country := ""
				response, err = requests.Get("https://opendata.baidu.com/api.php?query="+ip[0]+"&co=&resource_id=6006&oe=utf8", nil)
				if err == nil {
					obj, e := response.Json()
					if e == nil {
						if status, ok := obj["status"].(string); ok && status == "0" {
							country = obj["data"].([]interface{})[0].(map[string]interface{})["location"].(string)
						}
					}
				}

				fmt.Println(vars.I18n("IP") + ": " + ip[0] + ", " + country)
			}
		}
	} else {
		fmt.Println("🚫🚫🚫 " + vars.I18n("NETWORK_DISCONNECTED") + " 🚫🚫🚫")
	}
}

func LoadEnvVar(key, defaultValue string) string {
	value, exists := os.LookupEnv(key)
	if !exists {
		value = defaultValue
	}
	return value
}

func LoadEnvInt(key string, defaultValue int) int {
	value, exists := os.LookupEnv(key)
	if !exists || value == "" {
		return defaultValue
	}
	result, err := strconv.Atoi(value)
	if err != nil {
		logrus.Error(err)
		os.Exit(-1)
	}
	return result
}
