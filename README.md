# devbox

[Documentation](https://github.com/nicoalimin/devbox)

This is a tool for creating automated AI coding agents. It works by connecting to Linear issues, creating git worktrees for development, then using OpenCode to create and run sessions that complete the task.

## Features

- Works directly with Linear issues
- Uses GitHub repositories
- Creates automated pull requests from AI work
- Implements full CI/CD validation before PR creation

## Requirements

- Go 1.20+
- GitHub account with repository access
- Linear API key
- OpenCode server