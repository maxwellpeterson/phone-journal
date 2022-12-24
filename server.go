package main

import (
	"log"
	"net/http"
	"time"

	"github.com/caarlos0/env"
	"github.com/dstotijn/go-notion"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/twilio/twilio-go/client"
	"github.com/twilio/twilio-go/twiml"
)

const (
	transcriptPath = "/transcript"
)

type config struct {
	ExternalHostname string   `env:"EXTERNAL_HOSTNAME,required"`
	CallerWhitelist  []string `env:"CALLER_WHITELIST,required"`
	TwilioAuthToken  string   `env:"TWILIO_AUTH_TOKEN,required"`
	NotionAuthToken  string   `env:"NOTION_AUTH_TOKEN,required"`
	NotionDatabaseId string   `env:"NOTION_DATABASE_ID,required"`
}

func main() {
	cfg := config{}
	if err := env.Parse(&cfg); err != nil {
		log.Fatal(errors.Wrap(err, "parse env failed"))
	}

	router := gin.Default()
	router.SetTrustedProxies(nil)
	router.TrustedPlatform = gin.PlatformCloudflare

	requestValidator := client.NewRequestValidator(cfg.TwilioAuthToken)
	signatureChecker := checkTwilioSignature(&requestValidator, cfg.ExternalHostname)
	whitelistChecker := checkCallerWhitelist(cfg.CallerWhitelist)

	router.POST("/call", signatureChecker, whitelistChecker, func(c *gin.Context) {
		say := &twiml.VoiceSay{
			Message: "What's on your mind? This call is recorded.",
		}
		record := &twiml.VoiceRecord{
			TranscribeCallback: "https://" + cfg.ExternalHostname + transcriptPath,
		}

		twimlResult, err := twiml.Voice([]twiml.Element{say, record})
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
		} else {
			c.Header("Content-Type", "text/xml")
			c.String(http.StatusOK, twimlResult)
		}
	})

	router.POST(transcriptPath, signatureChecker, whitelistChecker, func(c *gin.Context) {
		c.Request.ParseForm()
		transcript := c.Request.PostForm.Get("TranscriptionText")

		client := notion.NewClient(cfg.NotionAuthToken)
		_, err := client.CreatePage(c.Request.Context(), notion.CreatePageParams{
			ParentType: notion.ParentTypeDatabase,
			ParentID:   cfg.NotionDatabaseId,
			DatabasePageProperties: &notion.DatabasePageProperties{
				"Date": notion.DatabasePageProperty{
					Date: &notion.Date{
						Start: notion.NewDateTime(time.Now(), false),
					},
				},
				"Title": notion.DatabasePageProperty{
					Title: []notion.RichText{
						{Text: &notion.Text{Content: "Test!"}},
					},
				},
			},
			Children: []notion.Block{
				notion.ParagraphBlock{RichText: []notion.RichText{
					{Text: &notion.Text{Content: transcript}},
				}},
			},
		})
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
		} else {
			c.String(http.StatusOK, "Thanks!")
		}
	})

	router.Run(":80")
}

// Snippet adapted from:
// https://www.twilio.com/docs/usage/tutorials/how-to-secure-your-gin-project-by-validating-incoming-twilio-requests
func checkTwilioSignature(validator *client.RequestValidator, hostname string) gin.HandlerFunc {
	return func(c *gin.Context) {
		url := "https://" + hostname + c.Request.URL.Path
		signature := c.Request.Header.Get("X-Twilio-Signature")

		c.Request.ParseForm()
		params := map[string]string{}
		for key, values := range c.Request.PostForm {
			if len(values) != 1 {
				c.AbortWithStatus(http.StatusBadRequest)
				return
			}
			params[key] = values[0]
		}

		if !validator.Validate(url, params, signature) {
			c.AbortWithStatus(http.StatusForbidden)
		} else {
			c.Next()
		}
	}
}

func checkCallerWhitelist(callerWhitelist []string) gin.HandlerFunc {
	allowed := map[string]bool{}
	for _, num := range callerWhitelist {
		allowed[num] = true
	}

	return func(c *gin.Context) {
		c.Request.ParseForm()
		caller := c.Request.PostForm.Get("From")
		if !allowed[caller] {
			twimlResult, err := twiml.Voice([]twiml.Element{&twiml.VoiceReject{}})
			if err != nil {
				c.AbortWithError(http.StatusInternalServerError, err)
			} else {
				c.Header("Content-Type", "text/xml")
				c.String(http.StatusOK, twimlResult)
			}
		} else {
			c.Next()
		}
	}
}
