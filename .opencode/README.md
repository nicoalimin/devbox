# OpenCode Agent Instructions

This folder contains instructions and guidelines for the OpenCode coding agent when working on Linear issues.

## Files

- **AGENTS.md**: Comprehensive agent behavior guide for Linear integration
  - How to search Notion for product context
  - When to ask for clarification vs. implementing
  - How to update Linear status during work
  - How to leave PR comments in Linear
  - Best practices and examples

## Purpose

When the n8n adapter assigns a Linear issue to OpenCode, these instructions guide the agent to:

1. **Proactively gather context** from Notion before coding
2. **Ask for clarification** when requirements are unclear
3. **Update Linear status** as work progresses
4. **Document work** with PR links and summaries in Linear
5. **Follow best practices** for code quality and testing

## Usage

OpenCode should automatically load these instructions when starting work. If not, the n8n workflow includes explicit instructions to review this folder.

## Maintenance

Update `AGENTS.md` when:
- Linear workflow states change
- New Notion workspaces are added
- Team structure changes
- Best practices evolve

Keep instructions clear, actionable, and with examples.
