// Package branch provides use-cases for creating or updating a branch.
package commit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/google/wire"

	"github.com/int128/ghcp/pkg/fs"
	"github.com/int128/ghcp/pkg/git"
	"github.com/int128/ghcp/pkg/git/commitstrategy"
	"github.com/int128/ghcp/pkg/github"
	"github.com/int128/ghcp/pkg/usecases/gitobject"
)

var Set = wire.NewSet(
	wire.Struct(new(Commit), "*"),
	wire.Bind(new(Interface), new(*Commit)),
)

type Interface interface {
	Do(ctx context.Context, in Input) error
}

type Input struct {
	TargetRepository git.RepositoryID
	TargetBranchName git.BranchName // if empty, target is the default branch
	ParentRepository git.RepositoryID
	CommitStrategy   commitstrategy.CommitStrategy
	CommitMessage    git.CommitMessage
	Author           *git.CommitAuthor // optional
	Committer        *git.CommitAuthor // optional
	Paths            []string          // if empty or nil, create an empty commit
	NoFileMode       bool
	DryRun           bool

	ForceUpdate bool //TODO: support force-update as well
}

// Commit commits files to the default/given branch on the repository.
type Commit struct {
	CreateGitObject gitobject.Interface
	FileSystem      fs.Interface
	GitHub          github.Interface
}

func (u *Commit) Do(ctx context.Context, in Input) error {
	if !in.TargetRepository.IsValid() {
		return errors.New("you must set GitHub repository")
	}
	if in.CommitMessage == "" {
		return errors.New("you must set commit message")
	}

	files, err := u.FileSystem.FindFiles(in.Paths, pathFilter{})
	if err != nil {
		return fmt.Errorf("could not find files: %w", err)
	}
	if len(in.Paths) > 0 && len(files) == 0 {
		return errors.New("no file exists in given paths")
	}

	if in.TargetBranchName == "" {
		q, err := u.GitHub.QueryDefaultBranch(ctx, github.QueryDefaultBranchInput{
			HeadRepository: in.TargetRepository,
			BaseRepository: in.ParentRepository, // mandatory but not used
		})
		if err != nil {
			return fmt.Errorf("could not determine the default branch: %w", err)
		}
		in.TargetBranchName = q.HeadDefaultBranchName
	}

	q, err := u.GitHub.QueryForCommit(ctx, github.QueryForCommitInput{
		ParentRepository: in.ParentRepository,
		ParentRef:        in.CommitStrategy.RebaseUpstream(), // valid only if rebase
		TargetRepository: in.TargetRepository,
		TargetBranchName: in.TargetBranchName,
	})
	if err != nil {
		return fmt.Errorf("could not find the repository: %w", err)
	}
	slog.Info("Author and committer", "user", q.CurrentUserName)
	if q.TargetBranchExists() {
		if err := u.updateExistingBranch(ctx, in, files, q); err != nil {
			return fmt.Errorf("could not update the existing branch (%s): %w", in.TargetBranchName, err)
		}
		return nil
	}
	if err := u.createNewBranch(ctx, in, files, q); err != nil {
		return fmt.Errorf("could not create a branch (%s) based on the default branch: %w", in.TargetBranchName, err)
	}
	return nil
}

type pathFilter struct{}

func (f pathFilter) SkipDir(path string) bool {
	base := filepath.Base(path)
	if base == ".git" {
		slog.Debug("Exclude .git directory", "path", path)
		return true
	}
	return false
}

func (f pathFilter) ExcludeFile(string) bool {
	return false
}

func (u *Commit) createNewBranch(ctx context.Context, in Input, files []fs.File, q *github.QueryForCommitOutput) error {
	gitObj := gitobject.Input{
		Files:         files,
		Repository:    in.TargetRepository,
		CommitMessage: in.CommitMessage,
		Author:        in.Author,
		Committer:     in.Committer,
		NoFileMode:    in.NoFileMode,
	}
	switch {
	case in.CommitStrategy.IsFastForward():
		slog.Info("Creating a branch", "branch", in.TargetBranchName)
		gitObj.ParentCommitSHA = q.ParentDefaultBranchCommitSHA
		gitObj.ParentTreeSHA = q.ParentDefaultBranchTreeSHA
	case in.CommitStrategy.IsRebase():
		slog.Info("Creating a branch", "branch", in.TargetBranchName, "ref", in.CommitStrategy.RebaseUpstream())
		gitObj.ParentCommitSHA = q.ParentRefCommitSHA
		gitObj.ParentTreeSHA = q.ParentRefTreeSHA
	case in.CommitStrategy.NoParent():
		slog.Info("Creating a branch with no parent", "branch", in.TargetBranchName)
	default:
		return fmt.Errorf("unknown commit strategy %+v", in.CommitStrategy)
	}

	slog.Debug("Creating a commit", "files", len(gitObj.Files))
	commit, err := u.CreateGitObject.Do(ctx, gitObj)
	if err != nil {
		return fmt.Errorf("error while creating a commit: %w", err)
	}
	slog.Info("Created a commit", "changedFiles", commit.ChangedFiles)
	if len(files) > 0 && commit.ChangedFiles == 0 {
		slog.Warn("Nothing to commit because the branch has the same file(s)")
		return nil
	}
	if in.DryRun {
		slog.Info("Do not create a branch due to dry-run", "branch", in.TargetBranchName)
		return nil
	}

	slog.Debug("Creating a branch", "branch", in.TargetBranchName)
	createBranchIn := github.CreateBranchInput{
		RepositoryNodeID: q.TargetRepositoryNodeID,
		BranchName:       in.TargetBranchName,
		CommitSHA:        commit.CommitSHA,
	}
	if err := u.GitHub.CreateBranch(ctx, createBranchIn); err != nil {
		return fmt.Errorf("error while creating %s branch: %w", in.TargetBranchName, err)
	}
	slog.Info("Created a branch", "branch", in.TargetBranchName)
	return nil
}

func (u *Commit) updateExistingBranch(ctx context.Context, in Input, files []fs.File, q *github.QueryForCommitOutput) error {
	gitObj := gitobject.Input{
		Files:         files,
		Repository:    in.TargetRepository,
		CommitMessage: in.CommitMessage,
		Author:        in.Author,
		Committer:     in.Committer,
		NoFileMode:    in.NoFileMode,
	}
	switch {
	case in.CommitStrategy.IsFastForward():
		slog.Info("Updating the branch by fast-forward", "branch", in.TargetBranchName)
		gitObj.ParentCommitSHA = q.TargetBranchCommitSHA
		gitObj.ParentTreeSHA = q.TargetBranchTreeSHA
	case in.CommitStrategy.IsRebase():
		slog.Info("Rebasing the branch", "branch", in.TargetBranchName, "ref", in.CommitStrategy.RebaseUpstream())
		gitObj.ParentCommitSHA = q.ParentRefCommitSHA
		gitObj.ParentTreeSHA = q.ParentRefTreeSHA
	case in.CommitStrategy.NoParent():
		slog.Info("Updating the branch to a commit with no parent", "branch", in.TargetBranchName)
	default:
		return fmt.Errorf("unknown commit strategy %+v", in.CommitStrategy)
	}

	slog.Debug("Creating a commit", "files", len(gitObj.Files))
	commit, err := u.CreateGitObject.Do(ctx, gitObj)
	if err != nil {
		return fmt.Errorf("error while creating a commit: %w", err)
	}
	slog.Info("Created a commit", "changedFiles", commit.ChangedFiles)
	if len(files) > 0 && commit.ChangedFiles == 0 {
		slog.Warn("Nothing to commit because the branch has the same file(s)", "branch", in.TargetBranchName)
		return nil
	}
	if in.DryRun {
		slog.Info("Do not update branch due to dry-run", "branch", in.TargetBranchName)
		return nil
	}

	slog.Debug("Updating the branch", "branch", in.TargetBranchName)
	updateBranchIn := github.UpdateBranchInput{
		BranchRefNodeID: q.TargetBranchNodeID,
		CommitSHA:       commit.CommitSHA,
		Force:           in.ForceUpdate,
	}
	if err := u.GitHub.UpdateBranch(ctx, updateBranchIn); err != nil {
		return fmt.Errorf("error while updating %s branch: %w", in.TargetBranchName, err)
	}
	slog.Info("Updated the branch", "branch", in.TargetBranchName)
	return nil
}
