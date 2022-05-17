package github

import (
	"context"
	"fmt"
	"time"

	"github.com/google/go-github/v43/github"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/params/info"
	"github.com/openshift-pipelines/pipelines-as-code/pkg/provider"
)

const taskStatusTemplate = `
<table>
  <tr><th>Status</th><th>Duration</th><th>Name</th></tr>

{{- range $taskrun := .TaskRunList }}
<tr>
<td>{{ formatCondition $taskrun.Status.Conditions }}</td>
<td>{{ formatDuration $taskrun.Status.StartTime $taskrun.Status.CompletionTime }}</td><td>

{{ $taskrun.ConsoleLogURL }}

</td></tr>
{{- end }}
</table>`

func getCheckName(status provider.StatusOpts, pacopts *info.PacOpts) string {
	if pacopts.ApplicationName != "" {
		if status.OriginalPipelineRunName == "" {
			return pacopts.ApplicationName
		}
		return fmt.Sprintf("%s / %s", pacopts.ApplicationName, status.OriginalPipelineRunName)
	}
	return status.OriginalPipelineRunName
}

func (v *Provider) getExistingCheckRunID(ctx context.Context, runevent *info.Event, status provider.StatusOpts) (*int64, error) {
	res, _, err := v.Client.Checks.ListCheckRunsForRef(ctx, runevent.Organization, runevent.Repository,
		runevent.SHA, &github.ListCheckRunsOptions{
			AppID: v.ApplicationID,
		})
	if err != nil {
		return nil, err
	}
	if *res.Total == 0 {
		return nil, nil
	}

	for _, checkrun := range res.CheckRuns {
		if *checkrun.ExternalID == status.PipelineRunName {
			return checkrun.ID, nil
		}
	}

	return nil, nil
}

func (v *Provider) createCheckRunStatus(ctx context.Context, runevent *info.Event, pacopts *info.PacOpts, status provider.StatusOpts) (*int64, error) {
	now := github.Timestamp{Time: time.Now()}
	checkrunoption := github.CreateCheckRunOptions{
		Name:       getCheckName(status, pacopts),
		HeadSHA:    runevent.SHA,
		Status:     github.String("in_progress"),
		DetailsURL: github.String(pacopts.LogURL),
		ExternalID: github.String(status.PipelineRunName),
		StartedAt:  &now,
	}

	checkRun, _, err := v.Client.Checks.CreateCheckRun(ctx, runevent.Organization, runevent.Repository, checkrunoption)
	if err != nil {
		return nil, err
	}
	return checkRun.ID, nil
}

// getOrUpdateCheckRunStatus create a status via the checkRun API, which is only
// available with Github apps tokens.
func (v *Provider) getOrUpdateCheckRunStatus(ctx context.Context, runevent *info.Event, pacopts *info.PacOpts, status provider.StatusOpts) error {
	var err error
	var ok bool
	var checkRunID *int64

	vintf, ok := v.CheckRunIDS.Load(status.PipelineRunName)
	if ok {
		checkRunID, ok = vintf.(*int64)
		if !ok {
			return fmt.Errorf("api error: cannot convert checkrunid")
		}
	}
	if !ok {
		if checkRunID, _ = v.getExistingCheckRunID(ctx, runevent, status); checkRunID == nil {
			checkRunID, err = v.createCheckRunStatus(ctx, runevent, pacopts, status)
			if err != nil {
				return err
			}
		}
	}
	v.CheckRunIDS.Store(status.PipelineRunName, checkRunID)

	checkRunOutput := &github.CheckRunOutput{
		Title:   &status.Title,
		Summary: &status.Summary,
		Text:    &status.Text,
	}

	opts := github.UpdateCheckRunOptions{
		Name:   getCheckName(status, pacopts),
		Status: &status.Status,
		Output: checkRunOutput,
	}

	if status.DetailsURL != "" {
		opts.DetailsURL = &status.DetailsURL
	}

	// Only set completed-at if conclusion is set (which means finished)
	if status.Conclusion != "" && status.Conclusion != "pending" {
		opts.CompletedAt = &github.Timestamp{Time: time.Now()}
		opts.Conclusion = &status.Conclusion
	}

	_, _, err = v.Client.Checks.UpdateCheckRun(ctx, runevent.Organization, runevent.Repository, *checkRunID, opts)
	return err
}

// createStatusCommit use the classic/old statuses API which is available when we
// don't have a github app token
func (v *Provider) createStatusCommit(ctx context.Context, runevent *info.Event, pacopts *info.PacOpts, status provider.StatusOpts) error {
	var err error
	now := time.Now()
	switch status.Conclusion {
	case "skipped":
		status.Conclusion = "success" // We don't have a choice than setting as succes, no pending here.
	case "neutral":
		status.Conclusion = "success" // We don't have a choice than setting as succes, no pending here.
	}
	if status.Status == "in_progress" {
		status.Conclusion = "pending"
	}

	ghstatus := &github.RepoStatus{
		State:       github.String(status.Conclusion),
		TargetURL:   github.String(status.DetailsURL),
		Description: github.String(status.Title),
		Context:     github.String(getCheckName(status, pacopts)),
		CreatedAt:   &now,
	}

	if _, _, err := v.Client.Repositories.CreateStatus(ctx,
		runevent.Organization, runevent.Repository, runevent.SHA, ghstatus); err != nil {
		return err
	}
	if status.Status == "completed" && status.Text != "" && runevent.EventType == "pull_request" {
		_, _, err = v.Client.Issues.CreateComment(ctx, runevent.Organization, runevent.Repository,
			runevent.PullRequestNumber,
			&github.IssueComment{
				Body: github.String(fmt.Sprintf("%s<br>%s", status.Summary, status.Text)),
			},
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func (v *Provider) CreateStatus(ctx context.Context, runevent *info.Event, pacopts *info.PacOpts, statusOpts provider.StatusOpts) error {
	if v.Client == nil {
		return fmt.Errorf("cannot set status on github no token or url set")
	}

	switch statusOpts.Conclusion {
	case "success":
		statusOpts.Title = "Success"
		statusOpts.Summary = "has <b>successfully</b> validated your commit."
	case "failure":
		statusOpts.Title = "Failed"
		statusOpts.Summary = "has <b>failed</b>."
	case "skipped":
		statusOpts.Title = "Skipped"
		statusOpts.Summary = "is skipping this commit."
	case "neutral":
		statusOpts.Title = "Unknown"
		statusOpts.Summary = "doesn't know what happened with this commit."
	}

	if statusOpts.Status == "in_progress" {
		statusOpts.Title = "CI has Started"
		statusOpts.Summary = "is running."
	}

	onPr := ""
	if statusOpts.OriginalPipelineRunName != "" {
		onPr = "/" + statusOpts.OriginalPipelineRunName
	}
	statusOpts.Summary = fmt.Sprintf("%s%s %s", pacopts.ApplicationName, onPr, statusOpts.Summary)

	if runevent.Provider.InfoFromRepo {
		return v.createStatusCommit(ctx, runevent, pacopts, statusOpts)
	}
	return v.getOrUpdateCheckRunStatus(ctx, runevent, pacopts, statusOpts)
}
