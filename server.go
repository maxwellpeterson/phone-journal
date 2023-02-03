package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/caarlos0/env"
	"github.com/dstotijn/go-notion"
	"github.com/faiface/beep"
	bwav "github.com/faiface/beep/wav"
	"github.com/ggerganov/whisper.cpp/bindings/go/pkg/whisper"
	"github.com/gin-gonic/gin"
	gwav "github.com/go-audio/wav"
	ws "github.com/orcaman/writerseeker"
	"github.com/pkg/errors"
	"github.com/twilio/twilio-go/client"
	"github.com/twilio/twilio-go/twiml"
)

const (
	// File path for whisper model
	modelPath = "ggml-small.en.bin"
	// Whisper requires a single-channel audio file
	whisperNumChans = 1
	// Url path for recording callback
	recordingPath = "/recording"
	// Maximum length of title string used in Notion
	maxTitleLen = 32
)

type config struct {
	ExternalHostname string   `env:"EXTERNAL_HOSTNAME,required"`
	CallerWhitelist  []string `env:"CALLER_WHITELIST,required"`
	TwilioAccountSid string   `env:"TWILIO_ACCOUNT_SID,required"`
	TwilioAuthToken  string   `env:"TWILIO_AUTH_TOKEN,required"`
	NotionAuthToken  string   `env:"NOTION_AUTH_TOKEN,required"`
	NotionDatabaseId string   `env:"NOTION_DATABASE_ID,required"`
}

func main() {
	cfg := config{}
	if err := env.Parse(&cfg); err != nil {
		log.Fatal(err)
	}

	model, err := whisper.New(modelPath)
	if err != nil {
		log.Fatal(errors.Wrap(err, "create whisper model failed"))
	}
	defer model.Close()

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
			RecordingStatusCallback: "https://" + cfg.ExternalHostname + recordingPath,
		}

		twimlResult, err := twiml.Voice([]twiml.Element{say, record})
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
		} else {
			c.Header("Content-Type", "text/xml")
			c.String(http.StatusOK, twimlResult)
		}
	})

	router.POST(recordingPath, signatureChecker, whitelistChecker, func(c *gin.Context) {
		c.Request.ParseForm()

		status := c.Request.PostForm.Get("RecordingStatus")
		if status != "completed" {
			c.AbortWithError(http.StatusBadRequest, errors.New("incomplete recording"))
			return
		}

		recordingUrl := c.Request.PostForm.Get("RecordingUrl")
		recording, err := downloadRecording(cfg, recordingUrl)
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}

		resampled, err := resampleRecording(recording)
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}

		transcript, err := transcribeRecording(model, resampled)
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
		fmt.Printf("Transcript: %s\n", transcript)

		if err := uploadTranscript(c.Request.Context(), cfg, transcript); err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
		} else {
			c.String(http.StatusOK, "Thanks!")
		}
	})

	router.Run(":80")
}

func downloadRecording(cfg config, url string) (*bytes.Reader, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cfg.TwilioAccountSid, cfg.TwilioAuthToken)

	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", res.StatusCode)
	}

	recording, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if err := res.Body.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(recording), nil
}

func resampleRecording(recording io.ReadSeeker) (*bytes.Reader, error) {
	streamer, format, err := bwav.Decode(recording)
	if err != nil {
		return nil, err
	}
	defer streamer.Close()
	if format.NumChannels != whisperNumChans {
		err := fmt.Errorf("unsupported number of channels: %d", format.NumChannels)
		return nil, err
	}

	resampler := beep.Resample(3, format.SampleRate, whisper.SampleRate, streamer)
	resampled := ws.WriterSeeker{}
	err = bwav.Encode(&resampled, resampler, beep.Format{
		SampleRate:  whisper.SampleRate,
		NumChannels: format.NumChannels,
		Precision:   format.Precision,
	})
	if err != nil {
		return nil, err
	}
	return resampled.BytesReader(), nil
}

func transcribeRecording(model whisper.Model, recording io.ReadSeeker) (string, error) {
	dec := gwav.NewDecoder(recording)
	buf, err := dec.FullPCMBuffer()
	if err != nil {
		return "", err
	}
	data := buf.AsFloat32Buffer().Data

	context, err := model.NewContext()
	if err != nil {
		return "", err
	}
	if err := context.Process(data, nil); err != nil {
		return "", err
	}

	var sb strings.Builder
	for {
		segment, err := context.NextSegment()
		if err == io.EOF {
			break
		} else if err != nil {
			return "", err
		}
		sb.WriteString(segment.Text)
	}
	return sb.String(), nil
}

func uploadTranscript(ctx context.Context, cfg config, transcript string) error {
	notionClient := notion.NewClient(cfg.NotionAuthToken)
	_, err := notionClient.CreatePage(ctx, notion.CreatePageParams{
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
					{Text: &notion.Text{Content: transcriptTitle(transcript)}},
				},
			},
		},
		Children: []notion.Block{
			notion.ParagraphBlock{RichText: []notion.RichText{
				{Text: &notion.Text{Content: transcript}},
			}},
		},
	})
	return err
}

func transcriptTitle(transcript string) string {
	runes := []rune(transcript)
	if len(runes) <= maxTitleLen {
		return transcript
	}
	return string(runes[:maxTitleLen]) + "..."
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
