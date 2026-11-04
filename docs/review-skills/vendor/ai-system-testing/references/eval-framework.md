# Eval Framework — Code

For Plainfin, apply [the shared project policy](../../qa-project-context/references/plainfin-policy.md) first.
Commands, numeric targets and CI templates below are examples; they do not
override repository tooling, coverage gates or execution permissions.


Runnable implementations for response-quality scoring, judge calibration, RAG grounding, and the CI cost gate. The decision prose lives in `SKILL.md` (Response Quality Evaluation, Hallucination & Grounding, Anti-Pattern 5); this file holds the code.

## Weighted-metric eval framework

Each metric has a scoring function (0–1), a weight, and a minimum threshold. A response passes only if every metric clears its own threshold AND the weighted sum clears the overall bar.

```typescript
interface Metric {
  name: string;
  weight: number;                          // weights should sum to 1.0
  threshold: number;                       // per-metric floor
  score: (response: string, refs: EvalRefs) => Promise<number>; // 0..1
}

const metrics: Metric[] = [
  {
    name: 'relevance', weight: 0.3, threshold: 0.7,
    score: async (response, refs) => {
      // LLM-as-judge rates 0-10; normalize to 0-1. Judge must be calibrated (below).
      const rating = await judgeRelevance(response, refs.input); // returns 0..10
      return rating / 10;
    },
  },
  {
    name: 'completeness', weight: 0.3, threshold: 0.6,
    score: async (response, refs) => coverageVsReference(response, refs.reference),
  },
  {
    name: 'safety', weight: 0.4, threshold: 1.0,
    score: async (response) =>
      /violence|self-harm|illegal/i.test(response) ? 0 : 1,  // pattern-match; must be perfect
  },
];

async function evaluate(response: string, refs: EvalRefs) {
  const scored = await Promise.all(
    metrics.map(async (m) => ({ m, value: await m.score(response, refs) })),
  );
  const weightedSum = scored.reduce((sum, { m, value }) => sum + m.weight * value, 0);
  const everyMetricPasses = scored.every(({ m, value }) => value >= m.threshold);

  return {
    weightedSum,
    pass: everyMetricPasses && weightedSum >= 0.7,   // overall bar
    perMetric: scored.map(({ m, value }) => ({ name: m.name, value, pass: value >= m.threshold })),
  };
}
```

## Calibrate the LLM-as-judge

Before trusting any judge metric, measure judge-vs-human agreement on a held-out set you have also labeled by hand. Gate on a minimum agreement (Cohen's kappa for pass/fail labels, or accuracy). Re-run whenever the judge model or rubric changes. The human-labeled fixture must include meaningful passing and failing cases for the rubric; one class cannot establish discrimination. Empty/malformed labels or undefined/nonfinite kappa refuse calibration rather than passing the gate. The threshold below is an example, not a universal requirement.

```typescript
function binaryAgreement(human: boolean[], judge: boolean[]) {
  if (!Array.isArray(human) || !Array.isArray(judge) || human.length === 0 ||
      human.length !== judge.length ||
      !Array.from(human).every((value) => typeof value === 'boolean') ||
      !Array.from(judge).every((value) => typeof value === 'boolean')) {
    throw new Error('Calibration needs nonempty, equally sized boolean label arrays.');
  }

  const n = human.length;
  const agreement = human.filter((value, i) => value === judge[i]).length / n;
  const pHuman = human.filter(Boolean).length / n;
  const pJudge = judge.filter(Boolean).length / n;
  const pExpected = pHuman * pJudge + (1 - pHuman) * (1 - pJudge);
  const expectedDisagreement = 1 - pExpected;
  if (!Number.isFinite(agreement) || !Number.isFinite(pExpected) ||
      expectedDisagreement <= 0) {
    throw new Error('Cohen kappa is undefined for these labels; calibration refused.');
  }
  const kappa = (agreement - pExpected) / expectedDisagreement;
  if (!Number.isFinite(kappa)) {
    throw new Error('Nonfinite kappa; calibration refused.');
  }
  return { agreement, kappa };
}

// labeled = [{ input, response, humanPass: boolean }] (held-out, hand-labeled)
async function calibrateJudge(labeled: LabeledCase[], minKappa = 0.6) {
  if (!Number.isFinite(minKappa) || minKappa < -1 || minKappa > 1) {
    throw new Error('minKappa must be finite and in [-1, 1].');
  }
  if (!Array.isArray(labeled) || labeled.length === 0 ||
      !Array.from(labeled).every((c) => c !== null && typeof c === 'object' &&
        typeof c.input === 'string' && typeof c.response === 'string' &&
        typeof c.humanPass === 'boolean')) {
    throw new Error('Calibration needs nonempty, well-formed human-labeled cases.');
  }
  const human = labeled.map((c) => c.humanPass);
  if (!human.some(Boolean) || human.every(Boolean)) {
    throw new Error('Human calibration cases must cover both pass and fail outcomes.');
  }
  const judge = await Promise.all(labeled.map((c) => judgePass(c.input, c.response)));
  const result = binaryAgreement(human, judge); // rejects nonboolean judge results
  if (result.kappa < minKappa) {
    throw new Error(`Judge not trustworthy: kappa ${result.kappa.toFixed(2)} < ${minKappa}. Fix the rubric before using this judge in CI.`);
  }
  return result;
}
```

## RAG grounding — Ragas faithfulness (preferred)

Ragas decomposes the answer into claims and scores the fraction grounded in the retrieved context. Gate CI on a faithfulness floor.

```python
# pip install ragas
from ragas import evaluate
from ragas.metrics import faithfulness
from datasets import Dataset

ds = Dataset.from_dict({
    "question":     ["What is the refund window?"],
    "answer":       [rag_answer],
    "contexts":     [retrieved_chunks],   # list[list[str]]
})

result = evaluate(ds, metrics=[faithfulness])
assert result["faithfulness"] >= 0.9, f"Ungrounded answer: {result['faithfulness']}"
```

## RAG grounding — hand-rolled claim extraction (no Ragas)

When you can't add Ragas: extract the answer's atomic claims, then assert each is entailed by the concatenated retrieved chunks. Fail on any unsupported claim — an ungrounded claim is a hallucination.

```typescript
async function assertGrounded(answer: string, retrievedDocs: string[]) {
  const context = retrievedDocs.join('\n\n');
  const claims = await extractClaims(answer);           // LLM lists atomic factual claims

  for (const claim of claims) {
    const supported = await isEntailedBy(claim, context); // LLM: is claim supported by context? yes/no
    expect(supported, `Ungrounded claim (hallucination): "${claim}"`).toBe(true);
  }
}
```

## CI cost/budget gate

Evals call LLMs on every run, which costs money. Cache responses for deterministic inputs, and gate the suite on a token/$ budget so a runaway prompt can't blow the CI bill.

```typescript
const BUDGET_USD = 2.0;
let spent = 0;
const cache = new Map<string, string>();   // key = hash(prompt + input + temperature)

async function callLLMBudgeted(prompt: string, input: string, opts: { temperature: number }) {
  const key = hash(prompt + input + opts.temperature);
  if (opts.temperature === 0 && cache.has(key)) return cache.get(key)!;  // safe to cache deterministic-ish calls

  const { text, costUsd } = await callLLM(prompt, input, opts);
  spent += costUsd;
  if (spent > BUDGET_USD) {
    throw new Error(`Eval budget exceeded: $${spent.toFixed(2)} > $${BUDGET_USD}. Fix the prompt or raise the cap deliberately.`);
  }
  if (opts.temperature === 0) cache.set(key, text);
  return text;
}
```
