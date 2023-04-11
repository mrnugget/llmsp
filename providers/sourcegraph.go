package providers

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pjlast/llmsp/claude"
	"github.com/pjlast/llmsp/sourcegraph/embeddings"
	"github.com/pjlast/llmsp/types"
	"github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

type SourcegraphLLM struct {
	FileMap          types.MemoryFileMap
	EmbeddingsClient *embeddings.Client
	ClaudeClient     *claude.Client
	URL              string
	AccessToken      string
	RepoID           string
	Mu               sync.Mutex
	Context          *struct {
		context.Context
		CancelFunc context.CancelFunc
	}
}

func getGitURL() string {
	out, err := exec.Command("git", "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (l *SourcegraphLLM) Initialize(settings types.LLMSPSettings) error {
	if settings.Sourcegraph == nil {
		return fmt.Errorf("Sourcegraph settings not present")
	}

	l.URL = settings.Sourcegraph.URL
	l.AccessToken = settings.Sourcegraph.AccessToken
	l.EmbeddingsClient = embeddings.NewClient(l.URL, l.AccessToken, nil)
	l.ClaudeClient = claude.NewClient(l.URL, l.AccessToken, nil)

	gitURL := getGitURL()
	if gitURL != "" {
		urlAndRepo := strings.Split(strings.Split(gitURL, "@")[1], ":")
		baseURL := urlAndRepo[0]
		repoName := baseURL + "/" + strings.TrimSuffix(urlAndRepo[1], ".git")

		for _, rn := range settings.Sourcegraph.RepoEmbeddings {
			if rn == repoName {
				repoID, err := l.EmbeddingsClient.GetRepoID(repoName)
				if err != nil {
				} else {
					l.RepoID = repoID
				}
			}
		}
	}

	return nil
}

func (l *SourcegraphLLM) GetCompletions(ctx context.Context, params types.CompletionParams) ([]lsp.CompletionItem, error) {
	l.Mu.Lock()
	if l.Context != nil {
		l.Context.CancelFunc()
	}
	ctx, cancel := context.WithCancel(ctx)

	l.Context = &struct {
		context.Context
		CancelFunc context.CancelFunc
	}{ctx, cancel}
	l.Mu.Unlock()
	time.Sleep(250 * time.Millisecond)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("context canceled")
	}

	startLine := params.Position.Line
	if params.Position.Line < 20 {
		startLine = 0
	}
	snippet := getFileSnippet(l.FileMap[params.TextDocument.URI], startLine, params.Position.Line)

	var embeddings *embeddings.EmbeddingsSearchResult = nil
	var err error
	if l.RepoID != "" {
		embeddings, _ = l.EmbeddingsClient.GetEmbeddings(l.RepoID, snippet, 8, 0)
	}
	claudeParams := claude.DefaultCompletionParameters(getMessages(embeddings))
	claudeParams.Messages = append(claudeParams.Messages,
		claude.Message{
			Speaker: "human",
			Text: fmt.Sprintf(`Here are the contents of the file you are working in:
%s`, l.FileMap[params.TextDocument.URI]),
		},
		claude.Message{
			Speaker: "assistant",
			Text:    "Ok.",
		},
		claude.Message{
			Speaker: "human",
			Text: fmt.Sprintf(`Suggest a code snippet to complete the following Go code. Provide only the suggestion, nothing else.
%s`, snippet),
		},
		claude.Message{
			Speaker: "assistant",
			Text:    "```go\n" + strings.Split(snippet, "\n")[len(strings.Split(snippet, "\n"))-1],
		})
	retChan, err := l.ClaudeClient.GetCompletion(ctx, claudeParams, false)
	if err != nil {
		return nil, err
	}
	var completion string
	for resp := range retChan {
		completion = resp
	}
	completion = strings.TrimSuffix(strings.TrimPrefix(completion, "```go\n"), "\n```")

	textEdit := &lsp.TextEdit{
		Range: lsp.Range{
			Start: lsp.Position{
				Line: params.Position.Line,
			},
			End: params.Position,
		},
		NewText: strings.Split(l.FileMap[params.TextDocument.URI], "\n")[params.Position.Line] + completion,
	}
	return []lsp.CompletionItem{
		{
			Label:    strings.TrimSpace(strings.Split(l.FileMap[params.TextDocument.URI], "\n")[params.Position.Line] + completion),
			Kind:     lsp.CIKSnippet,
			TextEdit: textEdit,
			Detail:   strings.Split(l.FileMap[params.TextDocument.URI], "\n")[params.Position.Line] + completion,
		},
	}, nil
}

func (l *SourcegraphLLM) GetCodeActions(doc lsp.DocumentURI, selection lsp.Range) []lsp.Command {
	commands := []lsp.Command{
		{
			Title:     "Provide suggestions",
			Command:   "suggest",
			Arguments: []interface{}{doc, selection.Start.Line, selection.End.Line},
		},
		{
			Title:     "Generate docstring",
			Command:   "docstring",
			Arguments: []interface{}{doc, selection.Start.Line, selection.End.Line},
		},
	}
	if strings.Contains(strings.Join(strings.Split(l.FileMap[doc], "\n")[selection.Start.Line:selection.End.Line+1], "\n"), "// TODO") {
		commands = append(commands, lsp.Command{
			Title:     "Implement TODOs",
			Command:   "todos",
			Arguments: []interface{}{doc, selection.Start.Line, selection.End.Line},
		})
	}
	if strings.Contains(strings.Join(strings.Split(l.FileMap[doc], "\n")[selection.Start.Line:selection.End.Line+1], "\n"), "// ASK") {
		commands = append(commands, lsp.Command{
			Title:     "Answer question",
			Command:   "answer",
			Arguments: []interface{}{doc, selection.Start.Line, selection.End.Line},
		})
	}
	return commands
}

func (l *SourcegraphLLM) ExecuteCommand(ctx context.Context, cmd lsp.Command, conn *jsonrpc2.Conn) error {
	switch cmd.Command {
	case "suggest":
		filename := lsp.DocumentURI(cmd.Arguments[0].(string))
		startLine := cmd.Arguments[1].(float64)
		endLine := cmd.Arguments[2].(float64)
		snippet := getFileSnippet(l.FileMap[filename], int(startLine), int(endLine))
		snippet = numberLines(snippet, int(startLine))
		return l.sendDiagnostics(ctx, conn, string(filename), snippet)

	case "docstring":
		filename := lsp.DocumentURI(cmd.Arguments[0].(string))
		startLine := int(cmd.Arguments[1].(float64))
		endLine := int(cmd.Arguments[2].(float64))
		funcSnippet := getFileSnippet(l.FileMap[filename], int(startLine), int(endLine))
		docstring := l.getDocString(funcSnippet)

		edits := []lsp.TextEdit{
			{
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      startLine,
						Character: 0,
					},
					End: lsp.Position{
						Line:      endLine,
						Character: len(strings.Split(l.FileMap[filename], "\n")[endLine]),
					},
				},
				NewText: docstring + "\n" + funcSnippet,
			},
		}

		editParams := types.ApplyWorkspaceEditParams{
			Edit: types.WorkspaceEdit{
				DocumentChanges: []types.TextDocumentEdit{
					{
						TextDocument: lsp.VersionedTextDocumentIdentifier{
							TextDocumentIdentifier: lsp.TextDocumentIdentifier{
								URI: filename,
							},
							Version: 0,
						},
						Edits: edits,
					},
				},
			},
		}

		conn.Notify(ctx, "workspace/applyEdit", editParams)
		return nil

	case "todos":
		filename := lsp.DocumentURI(cmd.Arguments[0].(string))
		startLine := int(cmd.Arguments[1].(float64))
		endLine := int(cmd.Arguments[2].(float64))
		funcSnippet := getFileSnippet(l.FileMap[filename], int(startLine), int(endLine))
		implemented := l.implementTODOs(l.FileMap[filename], funcSnippet)

		edits := []lsp.TextEdit{
			{
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      startLine,
						Character: 0,
					},
					End: lsp.Position{
						Line:      endLine,
						Character: len(strings.Split(l.FileMap[filename], "\n")[endLine]),
					},
				},
				NewText: "\n" + implemented,
			},
		}

		editParams := types.ApplyWorkspaceEditParams{
			Edit: types.WorkspaceEdit{
				DocumentChanges: []types.TextDocumentEdit{
					{
						TextDocument: lsp.VersionedTextDocumentIdentifier{
							TextDocumentIdentifier: lsp.TextDocumentIdentifier{
								URI: filename,
							},
							Version: 0,
						},
						Edits: edits,
					},
				},
			},
		}

		conn.Notify(ctx, "workspace/applyEdit", editParams)

	case "answer":
		filename := lsp.DocumentURI(cmd.Arguments[0].(string))
		startLine := int(cmd.Arguments[1].(float64))
		endLine := int(cmd.Arguments[2].(float64))
		funcSnippet := getFileSnippet(l.FileMap[filename], int(startLine), int(endLine))
		implemented := l.answerQuestions(l.FileMap[filename], funcSnippet)

		edits := []lsp.TextEdit{
			{
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      startLine,
						Character: 0,
					},
					End: lsp.Position{
						Line:      endLine,
						Character: len(strings.Split(l.FileMap[filename], "\n")[endLine]),
					},
				},
				NewText: "\n" + implemented,
			},
		}

		editParams := types.ApplyWorkspaceEditParams{
			Edit: types.WorkspaceEdit{
				DocumentChanges: []types.TextDocumentEdit{
					{
						TextDocument: lsp.VersionedTextDocumentIdentifier{
							TextDocumentIdentifier: lsp.TextDocumentIdentifier{
								URI: filename,
							},
							Version: 0,
						},
						Edits: edits,
					},
				},
			},
		}

		conn.Notify(ctx, "workspace/applyEdit", editParams)
	}
	return nil
}

func (l *SourcegraphLLM) implementTODOs(filecontents, function string) string {
	params := claude.DefaultCompletionParameters(getMessages(nil))
	params.Messages = append(params.Messages,
		claude.Message{
			Speaker: "human",
			Text: fmt.Sprintf(`Here are the contents of the file you are working in:
%s`, filecontents),
		},
		claude.Message{
			Speaker: "assistant",
			Text:    "Ok.",
		},
		claude.Message{
			Speaker: "human",
			Text: fmt.Sprintf(`The following Go code contains TODO instructions. Replace the TODO comments by implementing them. Import any Go libraries that would help complete the task. Only provide the completed code. Don't say anything else.
%s`, function),
		},
		claude.Message{
			Speaker: "assistant",
			Text:    "```go",
		})
	retChan, err := l.ClaudeClient.GetCompletion(context.Background(), params, true)
	if err != nil {
		return ""
	}
	var implemented string
	for resp := range retChan {
		implemented = resp
	}
	return strings.TrimSuffix(strings.TrimPrefix(implemented, "```go\n"), "\n```")
}

func (l *SourcegraphLLM) answerQuestions(filecontents, question string) string {
	question = strings.TrimPrefix(strings.TrimSpace(question), "// ASK: ")
	params := claude.DefaultCompletionParameters(getMessages(nil))
	params.Messages = append(params.Messages,
		claude.Message{
			Speaker: "human",
			Text: fmt.Sprintf(`Here are the contents of the file you are working in:
%s`, filecontents),
		},
		claude.Message{
			Speaker: "assistant",
			Text:    "Ok.",
		},
		claude.Message{
			Speaker: "human",
			Text: fmt.Sprintf(`Answer this question. Prepend each line with `+"`//`"+` since you are in a code editor.

%s`, question),
		},
		claude.Message{
			Speaker: "assistant",
			Text:    "// ANSWER: ",
		})
	retChan, err := l.ClaudeClient.GetCompletion(context.Background(), params, true)
	if err != nil {
		return ""
	}
	var answer string
	for resp := range retChan {
		answer = resp
	}
	return "// ASK: " + question + "\n" + answer
}

// sendDiagnostics sends the provided diagnostics back over the provided connection.
func (l *SourcegraphLLM) sendDiagnostics(ctx context.Context, conn jsonrpc2.JSONRPC2, filename, snippet string) error {
	repoID, err := l.EmbeddingsClient.GetRepoID("github.com/sourcegraph/sourcegraph")
	if err != nil {
		return err
	}
	var embeddingResults *embeddings.EmbeddingsSearchResult = nil
	if l.RepoID != "" {
		embeddingResults, _ = l.EmbeddingsClient.GetEmbeddings(repoID, snippet, 8, 0)
	}

	params := claude.DefaultCompletionParameters(getMessages(embeddingResults))
	params.Messages = append(params.Messages, getSuggestionMessages(strings.TrimPrefix(filename, "file://"), snippet)...)

	retChan, err := l.ClaudeClient.GetCompletion(ctx, params, true)

	for completionResp := range retChan {
		diagnostics := []lsp.Diagnostic{}
		for _, line := range strings.Split(completionResp, "\n") {
			parts := strings.Split(line, ": ")
			if len(parts) < 2 {
				continue
			}
			lineNumberRange := parts[0][5:]
			var lineStart, lineEnd int
			if strings.Contains(lineNumberRange, "-") {
				sp := strings.Split(lineNumberRange, "-")
				lineStart, err = strconv.Atoi(sp[0])
				if err != nil {
					continue
				}
				lineEnd, err = strconv.Atoi(sp[1])
				if err != nil {
					continue
				}
			} else {
				lineStart, err = strconv.Atoi(parts[0][5:])
				if err != nil {
					continue
				}
				lineEnd = lineStart
			}

			diagnostics = append(diagnostics, lsp.Diagnostic{
				Range: lsp.Range{
					Start: lsp.Position{
						Line:      lineStart,
						Character: 0,
					},
					End: lsp.Position{
						Line:      lineEnd,
						Character: 0,
					},
				},
				Severity: lsp.Log,
				Message:  parts[1],
			})
		}

		params := lsp.PublishDiagnosticsParams{
			URI:         lsp.DocumentURI(filename),
			Diagnostics: diagnostics,
		}
		if err := conn.Notify(ctx, "textDocument/publishDiagnostics", params); err != nil {
			return err
		}
	}

	return nil
}

func (l *SourcegraphLLM) getDocString(function string) string {
	params := claude.DefaultCompletionParameters(getMessages(nil))
	params.Messages = append(params.Messages, claude.Message{
		Speaker: "human",
		Text: fmt.Sprintf(`Generate a doc string explaining the use of the following Go function:
%s

Don't include the function in your output.`, function),
	},
		claude.Message{
			Speaker: "assistant",
			Text:    "//",
		})
	retChan, err := l.ClaudeClient.GetCompletion(context.Background(), params, true)
	if err != nil {
		return ""
	}
	var docstring string
	for resp := range retChan {
		docstring = resp
	}
	return docstring
}

func getFileSnippet(fileContent string, startLine, endLine int) string {
	fileLines := strings.Split(fileContent, "\n")
	return strings.Join(fileLines[startLine:endLine+1], "\n")
}

func numberLines(content string, startLine int) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		lines[i] = fmt.Sprintf("%d. %s", i+startLine, line)
	}
	return strings.Join(lines, "\n")
}

func getSuggestionMessages(filename, content string) []claude.Message {
	return []claude.Message{
		{
			Speaker: "human",
			Text: fmt.Sprintf(`Suggest improvements to following lines of code in the file '%s':
%s

Suggest improvements in the format:
Line {number}: {suggestion}`, filename, content),
		}, {
			Speaker: "assistant",
			Text:    "Line",
		},
	}
}

func getMessages(embeddingResults *embeddings.EmbeddingsSearchResult) []claude.Message {
	messages := []claude.Message{{
		Speaker: "assistant",
		Text: `I am Cody, an AI-powered coding assistant developed by Sourcegraph. I operate inside a Language Server Protocol implementation. My task is to help programmers with programming tasks in the Go programming language.
I am an expert in the Go programming language.
I have access to your currently open files in the editor.
I will generate suggestions as concisely and clearly as possible.
I only suggest something if I am certain about my answer.`,
	}}
	if embeddingResults != nil {
		for _, embedding := range embeddingResults.CodeResults {
			messages = append(messages, claude.Message{
				Speaker:  "human",
				FileName: embedding.FileName,
				Text: fmt.Sprintf(`Here are the contents of the file '%s':
%s`, embedding.FileName, embedding.Content),
			}, claude.Message{Speaker: "assistant", Text: "Ok."})
		}
	}

	return messages
}
