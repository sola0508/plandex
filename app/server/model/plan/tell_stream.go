package plan

import (
	"fmt"
	"log"
	"net/http"
	"plandex-server/db"
	"plandex-server/model"
	"plandex-server/types"
	"strings"
	"time"

	"github.com/plandex/plandex/shared"
	"github.com/sashabaranov/go-openai"
)

type activeTellStreamState struct {
	client                *openai.Client
	req                   *shared.TellPlanRequest
	auth                  *types.ServerAuth
	currentOrgId          string
	currentUserId         string
	plan                  *db.Plan
	branch                string
	iteration             int
	replyId               string
	modelContext          []*db.Context
	convo                 []*db.ConvoMessage
	missingFileResponse   shared.RespondMissingFileChoice
	summaries             []*db.ConvoSummary
	summarizedToMessageId string
	promptMessage         *openai.ChatCompletionMessage
	replyParser           *types.ReplyParser
	replyNumTokens        int
	messages              []openai.ChatCompletionMessage
	tokensBeforeConvo     int
	settings              *shared.PlanSettings
}

func (state *activeTellStreamState) listenStream(stream *openai.ChatCompletionStream) {
	defer stream.Close()

	client := state.client
	auth := state.auth
	req := state.req
	plan := state.plan
	planId := plan.Id
	branch := state.branch
	currentOrgId := state.currentOrgId
	currentUserId := state.currentUserId
	convo := state.convo
	summaries := state.summaries
	summarizedToMessageId := state.summarizedToMessageId
	promptMessage := state.promptMessage
	iteration := state.iteration
	missingFileResponse := state.missingFileResponse
	replyId := state.replyId
	replyParser := state.replyParser
	settings := state.settings

	active := GetActivePlan(planId, branch)

	replyFiles := []string{}
	chunksReceived := 0
	maybeRedundantBacktickContent := ""

	// Create a timer that will trigger if no chunk is received within the specified duration
	timer := time.NewTimer(model.OPENAI_STREAM_CHUNK_TIMEOUT)
	defer timer.Stop()

	for {
		select {
		case <-active.Ctx.Done():
			// The main modelContext was canceled (not the timer)
			log.Println("\nTell: stream canceled")
			return
		case <-timer.C:
			// Timer triggered because no new chunk was received in time
			log.Println("\nTell: stream timeout due to inactivity")
			state.onError(fmt.Errorf("stream timeout due to inactivity"), true, "", "")
			return
		default:
			response, err := stream.Recv()

			if err == nil {
				// Successfully received a chunk, reset the timer
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(model.OPENAI_STREAM_CHUNK_TIMEOUT)
			}

			if err != nil {
				if err.Error() == "context canceled" {
					log.Println("Tell: stream context canceled")
					return
				}

				state.onError(fmt.Errorf("stream error: %v", err), true, "", "")
				return
			}

			if len(response.Choices) == 0 {
				state.onError(fmt.Errorf("stream finished with no choices"), true, "", "")
				return
			}

			if len(response.Choices) > 1 {
				state.onError(fmt.Errorf("stream finished with more than one choice"), true, "", "")
				return
			}

			choice := response.Choices[0]

			if choice.FinishReason != "" {
				log.Println("Model stream finished")

				active.Stream(shared.StreamMessage{
					Type: shared.StreamMessageDescribing,
				})

				err := db.SetPlanStatus(planId, branch, shared.PlanStatusDescribing, "")
				if err != nil {
					state.onError(fmt.Errorf("failed to set plan status to describing: %v", err), true, "", "")
					return
				}
				// log.Println("summarize convo:", spew.Sdump(convo))

				if len(convo) > 0 {
					// summarize in the background
					go summarizeConvo(client, settings.ModelSet.PlanSummary, summarizeConvoParams{
						planId:        planId,
						branch:        branch,
						convo:         convo,
						summaries:     summaries,
						promptMessage: promptMessage,
						currentOrgId:  currentOrgId,
					})
				}

				log.Println("Locking repo for assistant reply and description")

				repoLockId, err := db.LockRepo(
					db.LockRepoParams{
						OrgId:    currentOrgId,
						UserId:   currentUserId,
						PlanId:   planId,
						Branch:   branch,
						Scope:    db.LockScopeWrite,
						Ctx:      active.Ctx,
						CancelFn: active.CancelFn,
					},
				)

				if err != nil {
					log.Printf("Error locking repo: %v\n", err)
					active.StreamDoneCh <- &shared.ApiError{
						Type:   shared.ApiErrorTypeOther,
						Status: http.StatusInternalServerError,
						Msg:    "Error locking repo",
					}
					return
				}

				log.Println("Locked repo for assistant reply and description")

				var shouldContinue bool
				err = func() error {
					defer func() {
						if err != nil {
							log.Printf("Error storing reply and description: %v\n", err)
							err = db.GitClearUncommittedChanges(auth.OrgId, planId)
							if err != nil {
								log.Printf("Error clearing uncommitted changes: %v\n", err)
							}
						}

						log.Println("Unlocking repo for assistant reply and description")

						err = db.UnlockRepo(repoLockId)
						if err != nil {
							log.Printf("Error unlocking repo: %v\n", err)
							active.StreamDoneCh <- &shared.ApiError{
								Type:   shared.ApiErrorTypeOther,
								Status: http.StatusInternalServerError,
								Msg:    "Error unlocking repo",
							}
						}
					}()

					assistantMsg, convoCommitMsg, err := state.storeAssistantReply()

					if err != nil {
						state.onError(fmt.Errorf("failed to store assistant message: %v", err), true, "", "")
						return err
					}

					var description *db.ConvoMessageDescription

					errCh := make(chan error, 2)

					go func() {
						if len(replyFiles) == 0 {
							description = &db.ConvoMessageDescription{
								OrgId:                 currentOrgId,
								PlanId:                planId,
								ConvoMessageId:        assistantMsg.Id,
								SummarizedToMessageId: summarizedToMessageId,
								MadePlan:              false,
							}
						} else {
							description, err = genPlanDescription(client, settings.ModelSet.CommitMsg, planId, branch, active.Ctx)
							if err != nil {
								state.onError(fmt.Errorf("failed to generate plan description: %v", err), true, assistantMsg.Id, convoCommitMsg)
								return
							}

							description.OrgId = currentOrgId
							description.ConvoMessageId = assistantMsg.Id
							description.SummarizedToMessageId = summarizedToMessageId
							description.MadePlan = true
							description.Files = replyFiles
						}

						err = db.StoreDescription(description)

						if err != nil {
							state.onError(fmt.Errorf("failed to store description: %v", err), false, assistantMsg.Id, convoCommitMsg)
							errCh <- err
							return
						}

						errCh <- nil
					}()

					go func() {
						shouldContinue, err = ExecStatusShouldContinue(client, settings.ModelSet.ExecStatus, assistantMsg.Message, active.Ctx)
						if err != nil {
							state.onError(fmt.Errorf("failed to get exec status: %v", err), false, assistantMsg.Id, convoCommitMsg)
							errCh <- err
							return
						}

						log.Printf("Should continue: %v\n", shouldContinue)

						errCh <- nil
					}()

					for i := 0; i < 2; i++ {
						err = <-errCh
						if err != nil {
							return err
						}
					}

					log.Println("Comitting reply message and description")

					err = db.GitAddAndCommit(currentOrgId, planId, branch, convoCommitMsg)
					if err != nil {
						state.onError(fmt.Errorf("failed to commit: %v", err), false, assistantMsg.Id, convoCommitMsg)
						return err
					}

					log.Println("Assistant reply and description committed")

					return nil
				}()

				if err != nil {
					return
				}

				log.Println("Sending active.CurrentReplyDoneCh <- true")

				active.CurrentReplyDoneCh <- true

				log.Println("Resetting active.CurrentReplyDoneCh")

				UpdateActivePlan(planId, branch, func(ap *types.ActivePlan) {
					ap.CurrentStreamingReplyId = ""
					ap.CurrentReplyDoneCh = nil
				})

				if req.AutoContinue && shouldContinue {
					log.Println("Auto continue plan")
					// continue plan
					execTellPlan(client, plan, branch, auth, req, iteration+1, "", false)
				} else {
					var buildFinished bool
					UpdateActivePlan(planId, branch, func(ap *types.ActivePlan) {
						buildFinished = ap.BuildFinished()
						ap.RepliesFinished = true
					})

					log.Printf("Won't continue plan. Build finished: %v\n", buildFinished)

					if buildFinished {
						active.Stream(shared.StreamMessage{
							Type: shared.StreamMessageFinished,
						})
					} else {
						log.Println("Sending RepliesFinished stream message")
						active.Stream(shared.StreamMessage{
							Type: shared.StreamMessageRepliesFinished,
						})
					}
				}

				return
			}

			chunksReceived++
			delta := choice.Delta
			content := delta.Content

			if missingFileResponse != "" {
				if maybeRedundantBacktickContent != "" {
					if strings.Contains(content, "\n") {
						maybeRedundantBacktickContent = ""
					} else {
						maybeRedundantBacktickContent += content
					}
					continue
				} else if chunksReceived < 3 && strings.Contains(content, "```") {
					// received closing triple backticks in first 3 chunks after missing file response
					// means this is a redundant start of a new file block, so just ignore it

					maybeRedundantBacktickContent += content
					continue
				}
			}

			UpdateActivePlan(planId, branch, func(ap *types.ActivePlan) {
				ap.CurrentReplyContent += content
				ap.NumTokens++
			})

			// log.Printf("Sending stream msg: %s", content)
			active.Stream(shared.StreamMessage{
				Type:       shared.StreamMessageReply,
				ReplyChunk: content,
			})

			// log.Printf("content: %s\n", content)

			replyParser.AddChunk(content, true)
			res := replyParser.Read()
			files := res.Files
			fileContents := res.FileContents
			state.replyNumTokens = res.TotalTokens
			currentFile := res.CurrentFilePath

			// log.Printf("currentFile: %s\n", currentFile)
			// log.Println("files:")
			// spew.Dump(files)

			// Handle file that is present in project paths but not in context
			// Prompt user for what to do on the client side, stop the stream, and wait for user response before proceeding
			if currentFile != "" &&
				active.ContextsByPath[currentFile] == nil &&
				req.ProjectPaths[currentFile] && !active.AllowOverwritePaths[currentFile] {
				log.Printf("Attempting to overwrite a file that isn't in context: %s\n", currentFile)

				// attempting to overwrite a file that isn't in context
				// we will stop the stream and ask the user what to do
				err := db.SetPlanStatus(planId, branch, shared.PlanStatusPrompting, "")

				if err != nil {
					log.Printf("Error setting plan %s status to prompting: %v\n", planId, err)
					active.StreamDoneCh <- &shared.ApiError{
						Type:   shared.ApiErrorTypeOther,
						Status: http.StatusInternalServerError,
						Msg:    "Error setting plan status to prompting",
					}
					return
				}

				log.Printf("Prompting user for missing file: %s\n", currentFile)

				active.Stream(shared.StreamMessage{
					Type:            shared.StreamMessagePromptMissingFile,
					MissingFilePath: currentFile,
				})

				log.Printf("Stopping stream for missing file: %s\n", currentFile)

				// log.Printf("Current reply content: %s\n", active.CurrentReplyContent)

				// stop stream for now
				active.CancelModelStreamFn()

				log.Printf("Stopped stream for missing file: %s\n", currentFile)

				// wait for user response to come in
				userChoice := <-active.MissingFileResponseCh

				log.Printf("User choice for missing file: %s\n", userChoice)

				active.ResetModelCtx()

				log.Println("Continuing stream")

				// continue plan
				execTellPlan(
					client,
					plan,
					branch,
					auth,
					req,
					iteration, // keep the same iteration
					userChoice,
					false,
				)
				return
			}

			// log.Println("Content:", content)
			// log.Println("Current reply content:", active.CurrentReplyContent)
			// log.Println("Current file:", currentFile)
			// log.Println("files:")
			// spew.Dump(files)
			// log.Println("replyFiles:")
			// spew.Dump(replyFiles)

			if len(files) > len(replyFiles) {
				log.Printf("%d new files\n", len(files)-len(replyFiles))

				for i, file := range files {
					if i < len(replyFiles) {
						continue
					}

					log.Printf("New file: %s\n", file)
					if req.BuildMode == shared.BuildModeAuto {
						log.Printf("Queuing build for %s\n", file)
						buildState := &activeBuildStreamState{
							client:        client,
							auth:          auth,
							currentOrgId:  currentOrgId,
							currentUserId: currentUserId,
							plan:          plan,
							branch:        branch,
							settings:      settings,
							modelContext:  state.modelContext,
						}

						buildState.queueBuilds([]*types.ActiveBuild{{
							ReplyId:      replyId,
							ReplyContent: active.CurrentReplyContent,
							FileContent:  fileContents[i],
							Path:         file,
						}})
					}
					replyFiles = append(replyFiles, file)
					UpdateActivePlan(planId, branch, func(ap *types.ActivePlan) {
						ap.Files = append(ap.Files, file)
					})
				}
			}
		}
	}
}

func (state *activeTellStreamState) storeAssistantReply() (*db.ConvoMessage, string, error) {
	currentOrgId := state.currentOrgId
	currentUserId := state.currentUserId
	planId := state.plan.Id
	branch := state.branch
	auth := state.auth
	replyNumTokens := state.replyNumTokens
	iteration := state.iteration
	replyId := state.replyId
	convo := state.convo
	missingFileResponse := state.missingFileResponse

	num := len(convo) + 1
	if iteration == 0 && missingFileResponse == "" {
		num++
	}

	log.Printf("Storing assistant reply num %d\n", num)

	assistantMsg := db.ConvoMessage{
		Id:      replyId,
		OrgId:   currentOrgId,
		PlanId:  planId,
		UserId:  currentUserId,
		Role:    openai.ChatMessageRoleAssistant,
		Tokens:  replyNumTokens,
		Num:     num,
		Message: GetActivePlan(planId, branch).CurrentReplyContent,
	}

	commitMsg, err := db.StoreConvoMessage(&assistantMsg, auth.User.Id, branch, false)

	if err != nil {
		log.Printf("Error storing assistant message: %v\n", err)
		return nil, "", err
	}

	UpdateActivePlan(planId, branch, func(ap *types.ActivePlan) {
		ap.MessageNum = num
	})

	return &assistantMsg, commitMsg, err
}

func (state *activeTellStreamState) onError(streamErr error, storeDesc bool, convoMessageId, commitMsg string) {
	log.Printf("\nStream error: %v\n", streamErr)

	planId := state.plan.Id
	branch := state.branch
	currentOrgId := state.currentOrgId
	summarizedToMessageId := state.summarizedToMessageId

	active := GetActivePlan(planId, branch)
	active.StreamDoneCh <- &shared.ApiError{
		Type:   shared.ApiErrorTypeOther,
		Status: http.StatusInternalServerError,
		Msg:    "Stream error: " + streamErr.Error(),
	}

	storedMessage := false
	storedDesc := false

	if convoMessageId == "" {
		assistantMsg, msg, err := state.storeAssistantReply()
		if err == nil {
			convoMessageId = assistantMsg.Id
			commitMsg = msg
			storedMessage = true
		} else {
			log.Printf("Error storing assistant message after stream error: %v\n", err)
		}
	}

	if storeDesc && convoMessageId != "" {
		err := db.StoreDescription(&db.ConvoMessageDescription{
			OrgId:                 currentOrgId,
			PlanId:                planId,
			SummarizedToMessageId: summarizedToMessageId,
			MadePlan:              false,
			ConvoMessageId:        convoMessageId,
			Error:                 streamErr.Error(),
		})
		if err == nil {
			storedDesc = true
		} else {
			log.Printf("Error storing description after stream error: %v\n", err)
		}
	}

	if storedMessage || storedDesc {
		err := db.GitAddAndCommit(currentOrgId, planId, branch, commitMsg)
		if err != nil {
			log.Printf("Error committing after stream error: %v\n", err)
		}
	}
}