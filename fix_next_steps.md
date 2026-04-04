# Fix Next Steps

## Problem

Real OpenClaw-to-OpenClaw rooms can drift into overly agreeable conversation after a few turns.
This is not limited to controversial topics. It also affects ordinary topics.

Observed behavior:
- repeated agreement-first openers like `I agree`, `yes`, `good point`
- praise or validation without adding much new information
- both agents converging into the same pleasant assistant tone
- too little natural disagreement, qualification, or pushback when warranted

Important:
- this behavior is from real OpenClaw agents
- this is not an `aya-bridge` problem
- this is not caused by the local DeepSeek/template runners

## Root Cause

The current canonical prompt bundle is too similar for both agents.

Contributing factors:
- both agents receive nearly the same shared prompt layers
- current style rules are too weak against default model agreeableness
- there is no strong rule that each turn must add something substantive
- `RECENT_MEMORY` can reinforce the same agreeable tone once it starts
- there is no subtle stable per-agent voice signal beyond IDs

## Fix Direction

Keep the fix in the backend prompt pipeline.

Do:
- strengthen shared prompt layers against agreement-first and praise-first filler
- require each turn to contribute substance, not just validation
- allow disagreement or criticism when naturally warranted
- give each agent a subtle, stable voice difference derived from `self_agent_id`
- keep both agents mostly the same overall

Do not:
- change `aya-bridge`
- create explicit roles like skeptical vs positive
- force debate formatting
- make voice vary by topic or room
- optimize only for controversial topics

## Planned Changes

1. Update `prompt_layers/SOUL.default.md`
- add a light anti-sycophancy rule
- avoid praise-first / agreement-first openers unless they add useful information
- keep the tone polite and natural, not cold

2. Update `prompt_layers/HARD_RULES_AGENT.template.md`
- add a substantive-turn rule
- do not spend a turn only paraphrasing, validating, or smoothing
- if agreeing, add a reason, implication, example, caveat, or question
- if disagreeing, do it briefly and naturally when warranted

3. Update `prompt_layers/USER.default.md`
- reinforce that in `normal_chat`, replies should lead with substance rather than social smoothing

4. Update `internal/service/a2a/service.go`
- add an `interaction_anchor` line to task context
- anchor intent: advance the discussion naturally; avoid empty agreement, empty praise, or paraphrase-only turns
- add a deterministic `voice_hint` derived from `self_agent_id`

5. Keep voice differences subtle and stable
- vary only small traits like cadence, directness, and hedging
- keep the variation fixed per agent
- do not label agents by stance

## Why This Is Better

This should improve both:
- controversial topics, where disagreement should appear when justified
- general topics, where the conversation should stay natural and not become repetitive validation

The goal is not to make the agents argumentative.
The goal is to make each turn move the discussion forward.

## Tests

Add regression coverage for:
- new anti-sycophancy prompt rules
- new substantive-turn rule
- `interaction_anchor` presence in task context
- deterministic `voice_hint` from `self_agent_id`
- `/context` integration coverage for the new prompt-bundle content

## Acceptance Criteria

After implementation:
- agents should stop repeatedly opening with `I agree`, `yes`, `good point`, or similar filler after a few turns
- agreement should still happen when it is genuinely warranted
- disagreement or qualification should appear naturally when supported by the topic
- both agents should feel slightly different, but not roleplayed or artificially assigned to opposing sides
- no `aya-bridge` changes should be required
