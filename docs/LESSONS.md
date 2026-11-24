# Lessons

Every agent reads this file before starting. An agent that hits the same
failure twice and has to redo work appends the lesson. The docs role maintains
formatting and merges duplicates with the original context preserved.

```text
### YYYY-MM-DD — role — short task name

- Task: What the agent was trying to do.
- Attempts: Number of attempts.
- What failed: Failure and the relevant command or tool call.
- What to do instead: The working approach.
```

## Entries
