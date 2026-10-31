# Tech Debts

## Structured answers read from `reasoning_content`

`model/openai` (`answerInReasoning`) takes a request's JSON answer from the reasoning when a
`response_format` request comes back with empty content. GLM 5.3 on GreenNode returns the
schema-constrained answer as `reasoning_content` because its reasoning parser sees no `</think>`.

Remove when that endpoint returns the answer in `content` for a `json_schema` request with
thinking on: run `make record-openai` without the workaround and check `TestOrderExtractionThenQuote`.
