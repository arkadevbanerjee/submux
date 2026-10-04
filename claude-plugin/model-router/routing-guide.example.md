# Routing guide

The model-router plugin adds this file to every session. It is judgment, not
code: edit it any time.

## Who decides
- The main loop's model and effort: me only.
- Before a model or effort change for a subagent, ask me, with your pick first.

## Factors
- Difficulty: trivial, routine, hard. It sets effort and model strength.
- Kind of work: search, code, review, plan. Use the cheapest model that is good enough.
- Quota left now, and when it resets.
- Provider health: recent errors or 429s this session.

## Escalation
- Weak answer: raise effort, then a stronger model.
- 429 or quota: the same model on another provider.

## Retired ids (spawns naming one are blocked)
- retired: example-model-1
