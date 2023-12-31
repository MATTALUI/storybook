package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/gofor-little/env"
	"github.com/google/uuid"
	"github.com/sashabaranov/go-openai"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/slides/v1"
	"io"
	"math/rand"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

type StorySynopsis struct {
	Animal string
	Name   string
	Goal   string
}

type Page struct {
	Id                uuid.UUID
	Paragraph         string
	ExcerptDescriptor string
	ImageDescriptor   string
	ImagePath         string
	PublicImagePath   string
}

type Story struct {
	Id             uuid.UUID
	Synopsis       StorySynopsis
	Paragraphs     []string
	RawGPTResponse string
	Pages          []Page
	Title          string
	CoverImage     string
}

type StabilityTextPrompt struct {
	Text   string `json:"text"`
	Weight int    `json:"weight"`
}

type StabilityRequestBody struct {
	Steps       int                   `json:"steps"`
	Width       int                   `json:"width"`
	Height      int                   `json:"height"`
	Seed        int                   `json:"seed"`
	CFGScale    int                   `json:"cfg_scale"`
	Samples     int                   `json:"samples"`
	TextPrompts []StabilityTextPrompt `json:"text_prompts"`
}

type StabilityResponseArtifact struct {
	Base64Image  string `json:"base64"`
	FinishReason string `json:"finishReason"`
	Seed         int    `json:"seed"`
}

type StabilityResponseBody struct {
	Artifacts []StabilityResponseArtifact `json:"artifacts"`
}

var (
	DEBUG                 bool
	OPEN_AI_KEY           string
	STABILITY_API_KEY     string
	S3_BUCKET_NAME        string
	AWS_ACCESS_KEY_ID     string
	AWS_SECRET_ACCESS_KEY string
	AWS_REGION            string
	FINAL_SLIDE_IMAGE     string
)

func init() {
	var err error

	env.Load("./.env")
	DEBUG = strings.ToLower(env.Get("DEBUG", "false")) == "true"
	OPEN_AI_KEY, err = env.MustGet("OPEN_AI_KEY")
	STABILITY_API_KEY, err = env.MustGet("STABILITY_API_KEY")
	S3_BUCKET_NAME, err = env.MustGet("S3_BUCKET_NAME")
	AWS_ACCESS_KEY_ID, err = env.MustGet("AWS_ACCESS_KEY_ID")
	AWS_SECRET_ACCESS_KEY, err = env.MustGet("AWS_SECRET_ACCESS_KEY")
	AWS_REGION, err = env.MustGet("AWS_REGION")
	FINAL_SLIDE_IMAGE, err = env.MustGet("FINAL_SLIDE_IMAGE")
	if err != nil {
		panic(err)
	}
}

func main() {
	banner, _ := os.ReadFile("./banner.txt")
	fmt.Println(string(banner))
	story := buildStory()
	collectSynopsisFromUser(story)
	getStoryFromGPT(story)
	extractParagraphs(story)
	buildCovers(story)

	count := len(story.Paragraphs)
	story.Pages = make([]Page, count)
	var wg sync.WaitGroup
	fmt.Println("Sweet. I think this could use some creative touches. Give me a moment...")
	for i, _ := range story.Paragraphs {
		wg.Add(1)
		go constructPage(i, story, &wg)
	}
	fmt.Println()
	wg.Wait()
	createSlideShow(story)
	fmt.Println("\nWe've done it.")
}

func exclaimRandomly() {
	exclamations := []string{
		"Oh yeah, this is looking good.",
		"I like this a lot.",
		"Wait, let me retry that one...",
		"Eh! Not too shabby!",
		"That's way better than I had imagined it.",
		"A little tweak here...",
		"A little rewrite there...",
		"Where did I put my pen?",
		"I'm going to ask my mom what she thinks of this real quick.",
		"It seems like I'm doing all the work here.",
		"Well, that's alright I guess.",
	}

	index := rand.Intn(len(exclamations))
	exclamation := exclamations[index]
	fmt.Println(exclamation)
}

func buildStory() *Story {
	story := Story{
		Id:         uuid.New(),
		Paragraphs: make([]string, 0),
		Title:      "Storybook Story",
		CoverImage: FINAL_SLIDE_IMAGE,
	}

	return &story
}

func collectSynopsisFromUser(story *Story) {
	// story.Synopsis.Animal = "zebra"
	// story.Synopsis.Name = "poncho"
	// story.Synopsis.Goal = "trying to become a giraffe"
	// return

	story.Synopsis = StorySynopsis{}
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Hello! Welcome to story book. Let's write a story together.")
	fmt.Println("Let's write a story about an animal.")
	fmt.Println("What kind of Animal should we write about?")
	fmt.Print("\n")

	rawAnimal, _ := reader.ReadString('\n')
	animal := strings.TrimSpace(rawAnimal)

	fmt.Printf("\nAh! %s! That's perfect!\n", animal)
	fmt.Printf("And what should we name this %s?\n\n", animal)

	rawName, _ := reader.ReadString('\n')
	name := strings.TrimSpace(rawName)

	fmt.Printf("\nA %s named %s. Interesting.\n", animal, name)
	fmt.Printf("What are %s's aspirations? Finish the sentence:\n", name)
	fmt.Printf("\"%s is trying to...\"\n\n", name)

	rawGoal, _ := reader.ReadString('\n')
	goal := strings.TrimSpace(rawGoal)

	fmt.Printf("\nOkay. %s is trying to %s.\n\n", name, goal)

	story.Synopsis.Animal = animal
	story.Synopsis.Name = name
	story.Synopsis.Goal = goal
}

func buildCovers(story *Story) {
	var wg sync.WaitGroup
	wg.Add(2)
	go getTitle(story, &wg)
	go getCoverImage(story, &wg)
	wg.Wait()
	fmt.Println("I think I've thought of a pretty good title")
}

func getTitle(story *Story, wg *sync.WaitGroup) {
	template := `Give me a potential title for the following short story about %s,
	a %s who is trying to %s.
	Do not give me a title with a subtitle. Format your response the following way:
	TITLE: "[title goes here]"
	"%s"`
	prompt := fmt.Sprintf(
		template,
		story.Synopsis.Name,
		story.Synopsis.Animal,
		story.Synopsis.Goal,
		story.RawGPTResponse,
	)
	resp, err := getGPTResponse(prompt)
	if err != nil {
		if DEBUG {
			panic(err)
		}
		fmt.Println("Screw it. I can't think of a title. No point in writing a story without a title")
		os.Exit(1)
	}
	properlyFormatted, err := regexp.MatchString(`^title: ".*"$`, strings.ToLower(strings.TrimSpace(resp)))
	if err != nil || !properlyFormatted {
		if DEBUG {
			fmt.Printf("properlyFormatted: %t\nresp: %s", properlyFormatted, resp)
			panic(err)
		}
		fmt.Println("Eh. I've got a foggy brain right now. I can't work like this. Goodbye.")
		os.Exit(1)
	}
	story.Title = strings.TrimSpace(strings.Split(resp, `"`)[1])
	wg.Done()
}

func getCoverImage(story *Story, wg *sync.WaitGroup) {
	template := `briefly describe a potential idea for the cover a childrens book about a %s named %s who is trying to %s`
	prompt := fmt.Sprintf(
		template,
		story.Synopsis.Animal,
		story.Synopsis.Name,
		story.Synopsis.Goal,
	)
	coverBaseDescription, err := getGPTResponse(prompt)
	if err != nil {
		if DEBUG {
			panic(err)
		}
		fmt.Println("I can't picture this anymore. Forget about it.")
		os.Exit(1)
	}
	coverDescription := fmt.Sprintf("in the style of a watercolor childrens book. %s", coverBaseDescription)
	results, err := getStabilityImages([]StabilityTextPrompt{
		{Text: coverDescription, Weight: 1},
		{Text: "writing words letters alphabet text", Weight: -1},
	})
	if err != nil {
		if DEBUG {
			panic(err)
		}
		fmt.Println("I messed up making the cover. It's worthless now.")
		os.Exit(1)
	}

	// Should only ever really be 1 here
	for _, result := range results.Artifacts {
		os.MkdirAll(fmt.Sprintf("./images/%s", story.Id), os.ModePerm)
		filePath := fmt.Sprintf("./images/%s/cover.png", story.Id)
		imageBytes, _ := base64.StdEncoding.DecodeString(result.Base64Image)
		f, _ := os.Create(filePath)
		f.Write(imageBytes)
		f.Close()
		f, _ = os.Open(filePath)
		uploader := getUploader()
		upload, err := uploader.Upload(&s3manager.UploadInput{
			Bucket: aws.String(S3_BUCKET_NAME),
			Key:    aws.String(fmt.Sprintf("DOCTOR_SLIDES_%s_cover.png", story.Id)),
			Body:   f,
		})
		if err != nil {
			if DEBUG {
				panic(err)
			}
			fmt.Println("You know, I am having trouble posting these images. Hrm. Try again later?")
			os.Exit(1)
		}
		story.CoverImage = upload.Location
	}

	wg.Done()
}

func getStoryFromGPT(story *Story) {
	// b, _ := os.ReadFile("./example.txt")
	// story.RawGPTResponse = string(b)
	// return
	fmt.Println("Let me think about how this story will go...")
	template := `Write me a short story in the style of a children's book about a
	%s named %s. %s is trying to %s. There should be a rising action, a climax,
	falling action, and a resolution. The story does not need to have a happy
	ending.`
	prompt := fmt.Sprintf(
		template,
		story.Synopsis.Animal,
		story.Synopsis.Name,
		story.Synopsis.Name,
		story.Synopsis.Goal,
	)
	resp, err := getGPTResponse(prompt)
	if err != nil {
		if DEBUG {
			panic(err)
		}
		fmt.Println("Hrm. I actually can't think of a story like that. Try again later!")
		os.Exit(1)
	}
	story.RawGPTResponse = resp
	fmt.Println("Okay. I think I have an idea.")
}

func extractParagraphs(story *Story) {
	fmt.Println("Let me edit it real quick...")
	lines := strings.Split(story.RawGPTResponse, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) > 0 {
			story.Paragraphs = append(story.Paragraphs, trimmed)
		}
	}
}

func constructPage(index int, story *Story, wg *sync.WaitGroup) {
	// We do a brief sleep in some of these so that we don't murder the API
	waitTime := rand.Intn(10) + 2
	time.Sleep(time.Duration(waitTime) * time.Second)

	newPage := Page{}
	newPage.Paragraph = story.Paragraphs[index]
	newPage.Id = uuid.New()
	story.Pages[index] = newPage

	buildPageDescriptors(index, story)
	getPageIllustration(index, story)
	uploadPublicImage(index, story)
	exclaimRandomly()

	wg.Done()
}

func getGPTResponse(message string) (string, error) {
	client := openai.NewClient(OPEN_AI_KEY)
	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: openai.GPT3Dot5Turbo,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: message,
				},
			},
		},
	)
	if err != nil {
		return "", err
	}
	return resp.Choices[0].Message.Content, nil
}
func buildPageDescriptors(index int, story *Story) {
	// return
	newPage := &story.Pages[index]
	excerptDescriptorTemplate := `
	The following is an excerpt from a childrens story about a(n) %s named
	%s who is trying to %s. Do not refer to %s by name. Given this excerpt write a brief
	(two sentence max) description of an illustration that would go well with
	this text.

	"%s"`
	newPage.ExcerptDescriptor = fmt.Sprintf(
		excerptDescriptorTemplate,
		story.Synopsis.Animal,
		story.Synopsis.Name,
		story.Synopsis.Goal,
		story.Synopsis.Name,
		newPage.Paragraph,
	)
	imageDescriptor, err := getGPTResponse(newPage.ExcerptDescriptor)
	if err != nil {
		if DEBUG {
			panic(err)
		}
		fmt.Println("I'm actually having a hard time picturing this. Let's try again later")
		os.Exit(1)
	}
	imageDescriptor = fmt.Sprintf("%s as a watercolor done in the style of a childrens book", imageDescriptor)
	imageDescriptor = strings.ToLower(imageDescriptor)
	imageDescriptor = strings.ReplaceAll(
		imageDescriptor,
		story.Synopsis.Name,
		fmt.Sprintf("the %s", story.Synopsis.Animal),
	)
	newPage.ImageDescriptor = imageDescriptor
}

func getStabilityImages(prompts []StabilityTextPrompt) (*StabilityResponseBody, error) {
	postUrl := "https://api.stability.ai/v1/generation/stable-diffusion-xl-1024-v1-0/text-to-image"
	bodyData := StabilityRequestBody{
		Steps:       40,
		Width:       1344,
		Height:      768,
		Seed:        0,
		CFGScale:    10,
		Samples:     1,
		TextPrompts: prompts,
	}
	postBody, _ := json.Marshal(bodyData)
	r, _ := http.NewRequest("POST", postUrl, bytes.NewBuffer(postBody))
	r.Header.Add("content-type", "application/json")
	r.Header.Add("Accept", "application/json")
	r.Header.Add("Stability-Client-ID", "storybook")
	r.Header.Add("Authorization", fmt.Sprintf("Bearer %s", STABILITY_API_KEY))
	client := &http.Client{}
	res, err := client.Do(r)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		if DEBUG {
			b, _ := io.ReadAll(res.Body)
			fmt.Println(res.StatusCode)
			fmt.Println(string(b))
			panic("Non-200 from Stability")
		}
		fmt.Println("I messed this painting up. Sorry.")
		os.Exit(1)
	}
	results := &StabilityResponseBody{}
	err = json.NewDecoder(res.Body).Decode(results)
	if err != nil {
		return nil, err
	}

	return results, nil
}

func getPageIllustration(index int, story *Story) {
	// story.Pages[index].ImagePath = fmt.Sprintf("./images/f672b210-047a-482a-8237-a0078a0cbb09/%d.png", index)
	// return
	newPage := &story.Pages[index]

	results, err := getStabilityImages([]StabilityTextPrompt{
		{Text: newPage.ImageDescriptor, Weight: 1},
	})
	if err != nil {
		if DEBUG {
			panic(err)
		}
		fmt.Println("This art didn't turn out the way I wanted. Maybe we should try again later.")
		os.Exit(1)
	}

	for _, result := range results.Artifacts {
		os.MkdirAll(fmt.Sprintf("./images/%s", story.Id), os.ModePerm)
		filePath := fmt.Sprintf("./images/%s/%d.png", story.Id, index)
		imageBytes, _ := base64.StdEncoding.DecodeString(result.Base64Image)
		f, _ := os.Create(filePath)
		f.Write(imageBytes)
		newPage.ImagePath = filePath
	}
}

func uploadPublicImage(index int, story *Story) {
	page := &story.Pages[index]
	uploader := getUploader()
	f, err := os.Open(page.ImagePath)
	if err != nil {
		if DEBUG {
			panic(err)
		}
		fmt.Println("Crap. I misplaced my art. Try again later?")
		os.Exit(1)
	}
	upload, err := uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(S3_BUCKET_NAME),
		Key:    aws.String(fmt.Sprintf("DOCTOR_SLIDES_%s_%d.png", story.Id, index)),
		Body:   f,
	})
	if err != nil {
		if DEBUG {
			panic(err)
		}
		fmt.Println("You know, I am having trouble posting these images. Hrm. Try again later?")
		os.Exit(1)
	}
	page.PublicImagePath = upload.Location
}

func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		fmt.Println("Unable to read authorization code")
	}

	tok, err := config.Exchange(oauth2.NoContext, authCode)
	if err != nil {
		fmt.Println("Unable to retrieve token from web")
	}

	return tok
}

func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	defer f.Close()
	if err != nil {
		return nil, err
	}
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)

	return tok, err
}

func saveToken(path string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	defer f.Close()
	if err != nil {
		fmt.Println("Unable to cache OAuth token")
	}
	json.NewEncoder(f).Encode(token)
}

func getGoogleClient() *http.Client {
	credsBytes, err := os.ReadFile("./credentials.json")
	if err != nil {
		panic(err)
	}
	config, err := google.ConfigFromJSON(credsBytes, "https://www.googleapis.com/auth/documents", "https://www.googleapis.com/auth/presentations", "https://www.googleapis.com/auth/spreadsheets")
	if err != nil {
		panic(err)
	}
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
}

func getUploader() *s3manager.Uploader {
	sess := session.Must(session.NewSession())
	uploader := s3manager.NewUploader(sess)

	return uploader
}

func createSlideShow(story *Story) {
	fmt.Println("Ah! That's perfect! Let me just put the finishing touches on it...")
	ctx := context.Background()
	client := getGoogleClient()
	slidesService, _ := slides.NewService(ctx, option.WithHTTPClient(client))

	presentation := &slides.Presentation{}
	presentation.Title = story.Title
	presentation.Layouts = []*slides.Page{
		{
			PageType: "LAYOUT",
		},
	}
	presentation, _ = slidesService.Presentations.Create(presentation).Do()
	updates := slides.BatchUpdatePresentationRequest{}
	updates.Requests = make([]*slides.Request, 0)
	updates.Requests = append(updates.Requests, buildTitleSlideUpdates(story)...)
	for index, page := range story.Pages {
		updates.Requests = append(updates.Requests, buildPageSlideUpdates(index, &page)...)
	}
	updates.Requests = append(updates.Requests, getFinalSlide()...)

	_, err := slidesService.Presentations.BatchUpdate(presentation.PresentationId, &updates).Do()
	if err != nil {
		panic(err)
	}
}

func buildTitleSlideUpdates(story *Story) []*slides.Request {
	// ctx := context.Background()
	// client := getGoogleClient()
	// slidesService, _ := slides.NewService(ctx, option.WithHTTPClient(client))

	// presentation := &slides.Presentation{}
	// presentation.Title = "Storybook Final Slide test"
	// presentation.Layouts = []*slides.Page{
	// 	{
	// 		PageType: "LAYOUT",
	// 	},
	// }
	// presentation, _ = slidesService.Presentations.Create(presentation).Do()
	updates := slides.BatchUpdatePresentationRequest{}
	updates.Requests = []*slides.Request{
		{
			CreateSlide: &slides.CreateSlideRequest{
				ObjectId: "titleSlide",
				SlideLayoutReference: &slides.LayoutReference{
					PredefinedLayout: "BLANK",
				},
			},
		},
		{
			DeleteObject: &slides.DeleteObjectRequest{
				ObjectId: "p", // this is the title given to the default slide
			},
		},
		{
			CreateImage: &slides.CreateImageRequest{
				ObjectId: "titlecoverimage",
				Url:      story.CoverImage,
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: "titleSlide",
					Transform: &slides.AffineTransform{
						ScaleX:     1.05,
						ScaleY:     1.05,
						TranslateX: 0.0,
						TranslateY: 0.0,
						Unit:       "PT",
					},
				},
			},
		},
		{
			CreateShape: &slides.CreateShapeRequest{
				ObjectId:  "titlebackground",
				ShapeType: "TEXT_BOX",
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: "titleSlide",
					Size: &slides.Size{
						Width:  &slides.Dimension{Magnitude: 720, Unit: "PT"},
						Height: &slides.Dimension{Magnitude: 405.64, Unit: "PT"},
					},
					// Transform: &slides.AffineTransform{
					// 	ScaleX:     1.0,
					// 	ScaleY:     1.0,
					// 	TranslateX: 15.0,
					// 	TranslateY: 15.0,
					// 	Unit:       "PT",
					// },
				},
			},
		},
		{
			UpdateShapeProperties: &slides.UpdateShapePropertiesRequest{
				ObjectId: "titlebackground",
				Fields:   "shapeBackgroundFill,outline,contentAlignment",
				ShapeProperties: &slides.ShapeProperties{
					ContentAlignment: "Middle",
					// Outline: &slides.Outline{
					// 	Weight:    &slides.Dimension{Magnitude: 1, Unit: "PT"},
					// 	DashStyle: "SOLID",
					// 	OutlineFill: &slides.OutlineFill{
					// 		SolidFill: &slides.SolidFill{
					// 			Color: &slides.OpaqueColor{
					// 				RgbColor: &slides.RgbColor{
					// 					Red:   0.35,
					// 					Green: 0.35,
					// 					Blue:  0.35,
					// 				},
					// 			},
					// 		},
					// 	},
					// },
					ShapeBackgroundFill: &slides.ShapeBackgroundFill{
						SolidFill: &slides.SolidFill{
							Alpha: 0.5,
							Color: &slides.OpaqueColor{
								RgbColor: &slides.RgbColor{
									Red:   0.37,
									Green: 0.37,
									Blue:  0.37,
								},
							},
						},
					},
				},
			},
		},
		{
			InsertText: &slides.InsertTextRequest{
				ObjectId: "titlebackground",
				Text:     story.Title,
			},
		},
		{
			UpdateParagraphStyle: &slides.UpdateParagraphStyleRequest{
				ObjectId: "titlebackground",
				Fields:   "alignment",
				Style: &slides.ParagraphStyle{
					Alignment: "Center",
				},
			},
		},
		{
			UpdateTextStyle: &slides.UpdateTextStyleRequest{
				ObjectId: "titlebackground",
				Fields:   "bold,fontSize,foregroundColor,fontFamily",
				Style: &slides.TextStyle{
					Bold:       true,
					FontSize:   &slides.Dimension{Magnitude: 80, Unit: "PT"},
					FontFamily: "Pacifico",
					ForegroundColor: &slides.OptionalColor{
						OpaqueColor: &slides.OpaqueColor{
							RgbColor: &slides.RgbColor{
								Red:   1.0,
								Green: 1.0,
								Blue:  1.0,
							},
						},
					},
				},
			},
		},
	}

	// _, err := slidesService.Presentations.BatchUpdate(presentation.PresentationId, &updates).Do()
	// if err != nil {
	// 	panic(err)
	// }

	return updates.Requests
}

func buildPageSlideUpdates(index int, page *Page) []*slides.Request {
	slideId := fmt.Sprintf("%d_SLIDE", index)
	paragraphId := fmt.Sprintf("%d_PARAGRAPH", index)
	imageId := fmt.Sprintf("%d_IMAGE", index)

	return []*slides.Request{
		{
			CreateSlide: &slides.CreateSlideRequest{
				ObjectId: slideId,
				SlideLayoutReference: &slides.LayoutReference{
					PredefinedLayout: "BLANK",
				},
			},
		},
		{
			CreateImage: &slides.CreateImageRequest{
				ObjectId: imageId,
				Url:      page.PublicImagePath,
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: slideId,
					Transform: &slides.AffineTransform{
						ScaleX:     1.05,
						ScaleY:     1.05,
						TranslateX: 0.0,
						TranslateY: 0.0,
						Unit:       "PT",
					},
				},
			},
		},
		{
			CreateShape: &slides.CreateShapeRequest{
				ObjectId:  paragraphId,
				ShapeType: "TEXT_BOX",
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: slideId,
					Size: &slides.Size{
						Width:  &slides.Dimension{Magnitude: 269, Unit: "PT"},
						Height: &slides.Dimension{Magnitude: 360, Unit: "PT"},
					},
					Transform: &slides.AffineTransform{
						ScaleX:     1.0,
						ScaleY:     1.0,
						TranslateX: 15.0,
						TranslateY: 15.0,
						Unit:       "PT",
					},
				},
			},
		},
		{
			UpdateShapeProperties: &slides.UpdateShapePropertiesRequest{
				ObjectId: paragraphId,
				Fields:   "shapeBackgroundFill,outline,contentAlignment",
				ShapeProperties: &slides.ShapeProperties{
					ContentAlignment: "TOP",
					Outline: &slides.Outline{
						Weight:    &slides.Dimension{Magnitude: 1, Unit: "PT"},
						DashStyle: "SOLID",
						OutlineFill: &slides.OutlineFill{
							SolidFill: &slides.SolidFill{
								Color: &slides.OpaqueColor{
									RgbColor: &slides.RgbColor{
										Red:   0.35,
										Green: 0.35,
										Blue:  0.35,
									},
								},
							},
						},
					},
					ShapeBackgroundFill: &slides.ShapeBackgroundFill{
						SolidFill: &slides.SolidFill{
							Alpha: 0.69,
							Color: &slides.OpaqueColor{
								RgbColor: &slides.RgbColor{
									Red:   0.37,
									Green: 0.37,
									Blue:  0.37,
								},
							},
						},
					},
				},
			},
		},
		{
			InsertText: &slides.InsertTextRequest{
				ObjectId: paragraphId,
				Text:     page.Paragraph,
			},
		},
		{
			UpdateParagraphStyle: &slides.UpdateParagraphStyleRequest{
				ObjectId: paragraphId,
				Fields:   "alignment",
				Style: &slides.ParagraphStyle{
					Alignment: "Start",
				},
			},
		},
		{
			UpdateTextStyle: &slides.UpdateTextStyleRequest{
				ObjectId: paragraphId,
				Fields:   "bold,fontSize,foregroundColor",
				Style: &slides.TextStyle{
					Bold:     true,
					FontSize: &slides.Dimension{Magnitude: 13, Unit: "PT"},
					ForegroundColor: &slides.OptionalColor{
						OpaqueColor: &slides.OpaqueColor{
							RgbColor: &slides.RgbColor{
								Red:   1.0,
								Green: 1.0,
								Blue:  1.0,
							},
						},
					},
				},
			},
		},
	}
}

func getFinalSlide() []*slides.Request {
	// ctx := context.Background()
	// client := getGoogleClient()
	// slidesService, _ := slides.NewService(ctx, option.WithHTTPClient(client))

	// presentation := &slides.Presentation{}
	// presentation.Title = "Storybook Final Slide test"
	// presentation.Layouts = []*slides.Page{
	// 	{
	// 		PageType: "LAYOUT",
	// 	},
	// }
	// presentation, _ = slidesService.Presentations.Create(presentation).Do()
	updates := slides.BatchUpdatePresentationRequest{}
	updates.Requests = []*slides.Request{
		{
			CreateSlide: &slides.CreateSlideRequest{
				ObjectId:       "finalSlide",
				InsertionIndex: 0,
				SlideLayoutReference: &slides.LayoutReference{
					PredefinedLayout: "BLANK",
				},
			},
		},
		{
			CreateImage: &slides.CreateImageRequest{
				ObjectId: "finalImage",
				Url:      FINAL_SLIDE_IMAGE,
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: "finalSlide",
					Transform: &slides.AffineTransform{
						ScaleX:     1.05,
						ScaleY:     1.05,
						TranslateX: 0.0,
						TranslateY: 0.0,
						Unit:       "PT",
					},
				},
			},
		},
		// I want this image to be transparent but its a readonly property in
		// the API. WTF?!
		// {
		// 	UpdateImageProperties: &slides.UpdateImagePropertiesRequest{
		// 		ObjectId: "finalImage",
		// 		Fields:   "transparency",
		// 		ImageProperties: &slides.ImageProperties{
		// 			Transparency: 0.69,
		// 		},
		// 	},
		// },
		{
			CreateShape: &slides.CreateShapeRequest{
				ObjectId:  "madeWith",
				ShapeType: "TEXT_BOX",
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: "finalSlide",
					Size: &slides.Size{
						Width:  &slides.Dimension{Magnitude: 163.44, Unit: "PT"},
						Height: &slides.Dimension{Magnitude: 38.16, Unit: "PT"},
					},
					Transform: &slides.AffineTransform{
						ScaleX:     1.0,
						ScaleY:     1.0,
						TranslateX: 23.75,
						TranslateY: 20.88,
						Unit:       "PT",
					},
				},
			},
		},
		{
			InsertText: &slides.InsertTextRequest{
				ObjectId: "madeWith",
				Text:     "Made With",
			},
		},
		{
			UpdateParagraphStyle: &slides.UpdateParagraphStyleRequest{
				ObjectId: "madeWith",
				Fields:   "alignment",
				Style: &slides.ParagraphStyle{
					Alignment: "Start",
				},
			},
		},
		{
			UpdateTextStyle: &slides.UpdateTextStyleRequest{
				ObjectId: "madeWith",
				Fields:   "bold,fontSize,fontFamily",
				Style: &slides.TextStyle{
					Bold:       false,
					FontSize:   &slides.Dimension{Magnitude: 19, Unit: "PT"},
					FontFamily: "Changa One",
				},
			},
		},
		{
			CreateShape: &slides.CreateShapeRequest{
				ObjectId:  "storyBook",
				ShapeType: "TEXT_BOX",
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: "finalSlide",
					Size: &slides.Size{
						Width:  &slides.Dimension{Magnitude: 543.6, Unit: "PT"},
						Height: &slides.Dimension{Magnitude: 130.32, Unit: "PT"},
					},
					Transform: &slides.AffineTransform{
						ScaleX:     1.0,
						ScaleY:     1.0,
						TranslateX: 0.0,
						TranslateY: 41.01,
						Unit:       "PT",
					},
				},
			},
		},
		{
			InsertText: &slides.InsertTextRequest{
				ObjectId: "storyBook",
				Text:     "Storybook",
			},
		},
		{
			UpdateParagraphStyle: &slides.UpdateParagraphStyleRequest{
				ObjectId: "storyBook",
				Fields:   "alignment",
				Style: &slides.ParagraphStyle{
					Alignment: "Start",
				},
			},
		},
		{
			UpdateTextStyle: &slides.UpdateTextStyleRequest{
				ObjectId: "storyBook",
				Fields:   "bold,fontSize,fontFamily",
				Style: &slides.TextStyle{
					Bold:       true,
					FontSize:   &slides.Dimension{Magnitude: 80, Unit: "PT"},
					FontFamily: "Pacifico",
				},
			},
		},
		{
			CreateShape: &slides.CreateShapeRequest{
				ObjectId:  "howitworks",
				ShapeType: "ROUND_RECTANGLE",
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: "finalSlide",
					Size: &slides.Size{
						Width:  &slides.Dimension{Magnitude: 223.2, Unit: "PT"},
						Height: &slides.Dimension{Magnitude: 44.64, Unit: "PT"},
					},
					Transform: &slides.AffineTransform{
						ScaleX:     1.0,
						ScaleY:     1.0,
						TranslateX: 23.76,
						TranslateY: 171.36,
						Unit:       "PT",
					},
				},
			},
		},
		{
			UpdateShapeProperties: &slides.UpdateShapePropertiesRequest{
				ObjectId: "howitworks",
				Fields:   "shapeBackgroundFill,link",
				ShapeProperties: &slides.ShapeProperties{
					Link: &slides.Link{
						Url: "https://github.com/MATTALUI/storybook",
					},
					ShapeBackgroundFill: &slides.ShapeBackgroundFill{
						SolidFill: &slides.SolidFill{
							Alpha: 0.85,
							Color: &slides.OpaqueColor{
								RgbColor: &slides.RgbColor{
									Red:   0.93,
									Green: 0.93,
									Blue:  0.93,
								},
							},
						},
					},
				},
			},
		},
		{
			InsertText: &slides.InsertTextRequest{
				ObjectId: "howitworks",
				Text:     "How It Works",
			},
		},
		{
			UpdateParagraphStyle: &slides.UpdateParagraphStyleRequest{
				ObjectId: "howitworks",
				Fields:   "alignment",
				Style: &slides.ParagraphStyle{
					Alignment: "Center",
				},
			},
		},
		{
			UpdateTextStyle: &slides.UpdateTextStyleRequest{
				ObjectId: "howitworks",
				Fields:   "bold,fontSize,fontFamily",
				Style: &slides.TextStyle{
					Bold:       false,
					FontSize:   &slides.Dimension{Magnitude: 14, Unit: "PT"},
					FontFamily: "Changa One",
				},
			},
		},
		{
			CreateShape: &slides.CreateShapeRequest{
				ObjectId:  "sourcelink",
				ShapeType: "ROUND_RECTANGLE",
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: "finalSlide",
					Size: &slides.Size{
						Width:  &slides.Dimension{Magnitude: 223.2, Unit: "PT"},
						Height: &slides.Dimension{Magnitude: 44.64, Unit: "PT"},
					},
					Transform: &slides.AffineTransform{
						ScaleX:     1.0,
						ScaleY:     1.0,
						TranslateX: 23.76,
						TranslateY: 225.36,
						Unit:       "PT",
					},
				},
			},
		},
		{
			UpdateShapeProperties: &slides.UpdateShapePropertiesRequest{
				ObjectId: "sourcelink",
				Fields:   "shapeBackgroundFill,link",
				ShapeProperties: &slides.ShapeProperties{
					Link: &slides.Link{
						Url: "https://github.com/MATTALUI/storybook",
					},
					ShapeBackgroundFill: &slides.ShapeBackgroundFill{
						SolidFill: &slides.SolidFill{
							Alpha: 0.85,
							Color: &slides.OpaqueColor{
								RgbColor: &slides.RgbColor{
									Red:   0.93,
									Green: 0.93,
									Blue:  0.93,
								},
							},
						},
					},
				},
			},
		},
		{
			InsertText: &slides.InsertTextRequest{
				ObjectId: "sourcelink",
				Text:     "Source",
			},
		},
		{
			UpdateParagraphStyle: &slides.UpdateParagraphStyleRequest{
				ObjectId: "sourcelink",
				Fields:   "alignment",
				Style: &slides.ParagraphStyle{
					Alignment: "Center",
				},
			},
		},
		{
			UpdateTextStyle: &slides.UpdateTextStyleRequest{
				ObjectId: "sourcelink",
				Fields:   "bold,fontSize,fontFamily",
				Style: &slides.TextStyle{
					Bold:       false,
					FontSize:   &slides.Dimension{Magnitude: 14, Unit: "PT"},
					FontFamily: "Changa One",
				},
			},
		},
		{
			CreateShape: &slides.CreateShapeRequest{
				ObjectId:  "versionlink",
				ShapeType: "ROUND_RECTANGLE",
				ElementProperties: &slides.PageElementProperties{
					PageObjectId: "finalSlide",
					Size: &slides.Size{
						Width:  &slides.Dimension{Magnitude: 223.2, Unit: "PT"},
						Height: &slides.Dimension{Magnitude: 44.64, Unit: "PT"},
					},
					Transform: &slides.AffineTransform{
						ScaleX:     1.0,
						ScaleY:     1.0,
						TranslateX: 23.76,
						TranslateY: 279.36,
						Unit:       "PT",
					},
				},
			},
		},
		{
			UpdateShapeProperties: &slides.UpdateShapePropertiesRequest{
				ObjectId: "versionlink",
				Fields:   "shapeBackgroundFill,link",
				ShapeProperties: &slides.ShapeProperties{
					Link: &slides.Link{
						Url: "https://github.com/MATTALUI/storybook",
					},
					ShapeBackgroundFill: &slides.ShapeBackgroundFill{
						SolidFill: &slides.SolidFill{
							Alpha: 0.85,
							Color: &slides.OpaqueColor{
								RgbColor: &slides.RgbColor{
									Red:   0.93,
									Green: 0.93,
									Blue:  0.93,
								},
							},
						},
					},
				},
			},
		},
		{
			InsertText: &slides.InsertTextRequest{
				ObjectId: "versionlink",
				Text:     "Version 0.1.0",
			},
		},
		{
			UpdateParagraphStyle: &slides.UpdateParagraphStyleRequest{
				ObjectId: "versionlink",
				Fields:   "alignment",
				Style: &slides.ParagraphStyle{
					Alignment: "Center",
				},
			},
		},
		{
			UpdateTextStyle: &slides.UpdateTextStyleRequest{
				ObjectId: "versionlink",
				Fields:   "bold,fontSize,fontFamily",
				Style: &slides.TextStyle{
					Bold:       false,
					FontSize:   &slides.Dimension{Magnitude: 14, Unit: "PT"},
					FontFamily: "Changa One",
				},
			},
		},
	}
	// _, err := slidesService.Presentations.BatchUpdate(presentation.PresentationId, &updates).Do()
	// if err != nil {
	// 	panic(err)
	// }

	return updates.Requests
}
