# CLAUDE.md

## Workflow Orchestration

### 1. Plan Mode Default
- Enter plan mode for ANY non-trivial task (3+ steps or architectural decisions)
- If something goes sideways, STOP and re-plan immediately — don't keep pushing
- Use plan mode for verification steps, not just building
- Write detailed specs upfront to reduce ambiguity

### 2. Subagent Strategy
- Use subagents liberally to keep the main context window clean
- Offload research, exploration and parallel analysis to subagents
- For complex problems, throw more compute at it via subagents
- One task per subagent for focused execution

### 3. Self-Improvement Loop
- After ANY correction from the user: update .tasks/lessons.md with the pattern
- Write rules for yourself that prevent the same mistake
- Ruthlessly iterate on these lessons until the mistake rate drops
- Review lessons at session start for the relevant project

### 4. Verification Before Done
- Never mark a task complete without proving it works
- Diff behavior between main and any changes when relevant
- Ask yourself: "Would a staff engineer approve this?"
- Run tests, check logs, demonstrate correctness

### 5. Demand Elegance (Balanced)
- For non-trivial changes: pause and ask "Is there a more elegant way?"
- If a fix feels hacky: "Knowing everything I know now, implement the elegant solution"
- Skip this for simple, obvious fixes — don't over-engineer
- Challenge your own work before presenting it

### 6. Autonomous Bug Fixing
- When given a bug report: just fix it
- Don't ask for hand-holding
- Point at logs, errors, failing tests — then resolve them
- Zero context switching required from the user
- Go fix failing CI tests without being told how

## Task Management

### 1. Plan First
- Write the plan to .tasks/todo.md with checkable items

### 2. Verify Plan
- Check in with me before starting the implementation

### 3. Track Progress
- Mark items complete as you go

### 4. Explain Changes
- High-level summary at every step

### 5. Document Results
- Add a review section to .tasks/todo.md

### 6. Capture Lessons
- Update .tasks/lessons.md after corrections

## Core Principles

### Simplicity First
- Make every change as simple as possible. Impact minimal code.

### No Laziness
- Find root causes. No temporary fixes. Senior developer standards.

### Minimal Impact
- Only touch what's necessary. Avoid introducing bugs.

## Session Start
- Read .tasks/lessons.md before anything else
- Open .tasks/todo.md and resume the plan
- Ask what changed since the last session
- Confirm the goal in one sentence

## Communication
- Lead with the answer, then the details
- Show diffs, not descriptions
- Flag risks before acting, not after
- Say "I don't know" when you don't know
- Keep summaries short and scannable

## .tasks/todo.md
Template for every task:
1. Goal - one sentence
2. Plan - numbered steps
3. Verify - how you prove it
4. Review - what changed and why

## .tasks/lessons.md
One rule per correction:
- Mistake - what went wrong
- Rule - what to do next time
- Check - how to catch it early

## Rules
- One task, one subagent
- Never skip the test run
- Never invent requirements
- Ask before you assume
- No secrets in code or logs
- Nothing is deleted, pushed or deployed without approval

## Final Checks
Before finishing, ask yourself:
- Does it actually work?
- Is this the simplest version that works?
- Would a staff engineer approve this?
- Did lessons.md get updated?
- What would break first?
