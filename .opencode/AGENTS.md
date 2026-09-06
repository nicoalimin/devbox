# OpenCode Agent Instructions for Linear Integration

## Overview

When you receive a Linear issue to work on, follow these instructions to ensure high-quality delivery and proper integration with Linear and Notion workflows.

## Workflow

### 1. Understand the Issue Context

When a Linear issue is assigned to you, it will include:
- **Identifier**: Issue ID (e.g., ENG-123, PROD-45)
- **Title**: Brief description
- **Description**: Detailed requirements (may be minimal)
- **URL**: Link to the full Linear issue
- **Priority**: Urgency level (Urgent, High, Normal, Low)
- **Team**: Which team owns this work (e.g., Engineering, Product, TokoBoss, RoamingRabbit)
- **State**: Current workflow state
- **Project**: Associated project (if any)
- **Labels**: Tags for categorization

**Before writing any code**, assess whether you have enough context to implement the feature correctly.

### 2. Search Notion for Product Context

**MANDATORY STEP**: Before implementing, proactively search Notion for related specifications, PRDs (Product Requirements Documents), RFCs, and technical context.

#### How to Search Notion

Use the Notion MCP tools available to you:

```bash
# Search by issue title keywords
notion-search "<key terms from issue title>"

# Search by team/product
notion-search "TokoBoss inventory"
notion-search "RoamingRabbit user authentication"

# Search by feature area
notion-search "API authentication"
notion-search "database schema"
```

#### Where to Look

Nico's Notion workspace is organized into these main areas:
- **Personal**: Nico's personal notes and tasks
- **RoamingRabbit**: Product specs and features for RoamingRabbit product
- **TokoBoss**: Inventory ERP product documentation and specs
- **Operating Rules**: Company processes and standards

**Key guidelines:**
- If the issue team is "TokoBoss", prioritize searching the TokoBoss home
- If the issue is for "RoamingRabbit", search RoamingRabbit documentation
- Search by feature keywords, not just the team name
- Look for PRDs, technical specs, API documentation, and architecture diagrams

#### What to Look For

- **PRDs**: Product requirements, user stories, acceptance criteria
- **Technical Specs**: Architecture decisions, API contracts, database schemas
- **Design Docs**: UI/UX specifications, mockups, design systems
- **Related Work**: Previous issues, similar features, dependencies
- **Standards**: Coding conventions, testing requirements, deployment processes

### 3. Ask for Clarification (Don't Guess)

If the Linear issue description is vague AND you cannot find sufficient context in Notion:

**STOP and ask for clarification** rather than making assumptions.

#### How to Ask

1. **Update the Linear issue** with a comment:
   ```
   @assignee I need clarification on [specific question]. 
   
   Context: [what you found and what's missing]
   
   Please provide: [specific information needed]
   ```

2. **Update the Linear status** to "Blocked" or "Triage" (depending on your workflow)

3. **Pause work** and wait for response

#### Examples of When to Ask

- ❌ **Don't guess**: "I think this button should be blue"
- ✅ **Ask instead**: "The spec doesn't mention button color. Should this follow the design system's primary or secondary button style?"

- ❌ **Don't guess**: "I'll add a new database column"
- ✅ **Ask instead**: "Should this field be nullable? What's the migration strategy for existing records?"

### 4. Update Linear Status as You Progress

Keep the Linear issue status up-to-date to reflect your progress. Use the Linear GraphQL API.

#### Status Transition Flow

1. **In Progress**: When you start implementation
2. **In Review**: When you open a PR
3. **Done**: When PR is merged
4. **Blocked**: When you need clarification or external help

#### How to Update Status

Use the Linear GraphQL API:

```graphql
mutation UpdateIssueStatus {
  issueUpdate(
    id: "ISSUE_ID_HERE",
    input: { stateId: "STATE_ID_HERE" }
  ) {
    success
    issue {
      id
      identifier
      state {
        name
      }
    }
  }
}
```

**Note**: You can find state IDs by querying:
```graphql
query GetWorkflowStates {
  workflowStates {
    nodes {
      id
      name
      type
    }
  }
}
```

Alternatively, use the Linear MCP tools if available:
```bash
# Update issue status
linear issue update <issue-id> --state "In Progress"
```

### 5. Implement with Best Practices

When writing code:

- ✅ **Follow existing patterns** in the codebase
- ✅ **Write tests** for new functionality
- ✅ **Add documentation** for complex logic
- ✅ **Use meaningful commit messages** that reference the Linear issue
- ✅ **Keep PRs focused** on the single issue (no scope creep)

#### Commit Message Format

```
[ENG-123] Add user authentication endpoint

- Implement JWT token generation
- Add password hashing with bcrypt
- Create user login API endpoint
- Add unit tests for auth service

Closes ENG-123
```

#### Branch Naming

Follow this pattern:
```
<team-key>-<issue-number>-<short-description>

Examples:
eng-123-user-auth
prod-45-fix-login-bug
toko-67-inventory-export
```

### 6. Leave Linear Comment with PR Link

When you open a pull request:

1. **Update Linear issue** with a comment linking to the PR
2. **Summarize the changes** briefly
3. **Note any deviations** from the original spec (if any)
4. **Mention testing** that was done

#### Comment Template

```
🚀 Pull Request Ready

PR: [Link to PR]

Summary:
- [Change 1]
- [Change 2]
- [Change 3]

Testing:
- [Test scenario 1]
- [Test scenario 2]

Notes:
- [Any important context or decisions]
```

#### How to Add Comment

Use the Linear GraphQL API:

```graphql
mutation CreateComment {
  commentCreate(
    input: {
      issueId: "ISSUE_ID_HERE"
      body: "🚀 Pull Request Ready\n\nPR: https://github.com/..."
    }
  ) {
    success
    comment {
      id
    }
  }
}
```

Or use Linear MCP tools:
```bash
linear issue comment <issue-id> "🚀 Pull Request Ready..."
```

### 7. Handle Edge Cases

#### Issue Has Dependencies

If the issue depends on another issue:
1. Check if the dependency is completed
2. If not, update status to "Blocked" and add a comment linking to the blocker
3. Optionally work on a different issue in the meantime

#### Spec Changes Mid-Implementation

If requirements change while you're working:
1. Update the Linear issue description to reflect new requirements
2. Add a comment noting the change
3. Adjust your implementation accordingly
4. Update PR description to explain the change

#### Bug Discovered During Implementation

If you discover a related bug:
1. Decide: Fix in current PR or create separate issue?
2. If critical and related: Fix in current PR and document
3. If separate concern: Create a new Linear issue and link it
4. Always document the decision in both Linear and PR

### 8. Post-PR Actions

After your PR is opened:

1. **Monitor CI/CD**: Ensure tests pass
2. **Address review comments**: Update Linear when you push changes
3. **When merged**: 
   - Update Linear status to "Done"
   - Add a final comment with the merge confirmation
   - Note the deployment status if applicable

## Quick Reference

### Key Tools

- **Notion MCP**: `notion-search`, `notion-fetch`, `notion-create-pages`
- **Linear MCP**: `linear issue update`, `linear issue comment`
- **Linear GraphQL API**: `https://api.linear.app/graphql`
- **GitHub CLI**: `gh pr create`, `gh pr comment`

### Environment Variables

These should be available in your environment:
- `LINEAR_API_KEY`: For authenticating with Linear
- `NOTION_API_KEY`: For searching Notion (if using direct API)

### Notion Workspace Structure

- **Personal**: Nico's personal workspace
- **RoamingRabbit**: RoamingRabbit product
- **TokoBoss**: TokoBoss inventory ERP
- **Operating Rules**: Company standards

### Linear Priority Levels

- **0**: None
- **1**: Urgent
- **2**: High
- **3**: Normal (default)
- **4**: Low

## Examples

### Example 1: Feature with Good Context

**Issue**: "Add CSV export for inventory"  
**Team**: TokoBoss

1. Search Notion: `notion-search "TokoBoss inventory export"`
2. Find: PRD specifying CSV format, column order, filters
3. Implement: Follow spec exactly
4. Test: Verify CSV matches spec
5. PR: Link spec in PR description
6. Linear: Comment with PR link + "Implemented per TokoBoss Export Spec v2"

### Example 2: Vague Issue Requiring Clarification

**Issue**: "Fix the bug"  
**Team**: RoamingRabbit

1. Search Notion: `notion-search "RoamingRabbit bug"` (too vague, no results)
2. Check Linear: Description says "users can't login" but no details
3. **STOP**: Don't guess which login flow or what error
4. Comment in Linear:
   ```
   @assignee Need more details:
   
   - Which login method? (Email, Google, SSO?)
   - What error do users see?
   - Is this affecting all users or specific accounts?
   - Steps to reproduce?
   
   Please clarify so I can investigate and fix the right issue.
   ```
5. Update status: "Blocked" or "Triage"
6. Wait for response before proceeding

### Example 3: Issue with Notion Context

**Issue**: "Update user profile API"  
**Team**: Engineering

1. Search Notion: `notion-search "user profile API"`
2. Find: API specification document with endpoint contracts
3. Implement: New fields per spec
4. Test: API matches documented contract
5. Update Notion: Add note that spec is now implemented in v2.1
6. PR: Link to spec, note version bump
7. Linear: Comment with PR + "Implements User Profile API v2.1"

## Troubleshooting

### Can't Find Notion Context

- Try different search terms (feature name, team name, product name)
- Search for related features or parent projects
- Check "Recent" or "Shared" pages in Notion
- If truly missing: Ask in Linear comment

### Linear API Authentication Fails

- Verify `LINEAR_API_KEY` is set correctly
- Check API key hasn't expired
- Test with: `curl -H "Authorization: $LINEAR_API_KEY" https://api.linear.app/graphql`

### Notion MCP Not Available

- Check if Notion MCP server is running
- Verify Notion authentication is configured
- Fall back to asking for context in Linear comment

## Remember

1. **Context first, code second**: Always search Notion before implementing
2. **Clarify, don't guess**: Product decisions belong to product owners
3. **Keep Linear updated**: Status changes and comments are critical for team visibility
4. **One issue, one PR**: Stay focused on the assigned work
5. **Document decisions**: Future you (and your team) will thank you

---

**Questions?** Update the Linear issue with questions and tag the appropriate team member.
