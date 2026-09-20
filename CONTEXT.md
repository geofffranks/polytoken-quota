# Quota-Aware Model Selection

Vocabulary for assessing subagent work and selecting suitable models using provider quota observations. These definitions describe the agreed selection domain, not implementation status.

## Language

### Task suitability

**Task assessment**:
An evaluation of the difficulty of the work requested from a subagent, distinct from choosing a model to perform it.
_Avoid_: Model selection when referring only to classification

**Difficulty tier**:
One of four ordered levels of task difficulty: Routine, Normal, Difficult, and Very Difficult. The tier represents the hardest required part of the work, not merely prompt length.

**Routine**:
Mechanical, well-specified work following an established procedure, without a harder requirement elsewhere in the task.

**Normal**:
Typical work with a clear approach and established patterns to follow.

**Difficult**:
Work requiring interpretation or novel decisions, including cross-cutting changes, performance or concurrency reasoning, and persisted-data implications.

**Very Difficult**:
Work involving architecture-level decisions, migration policy, security-sensitive surfaces, deep coupling, or novel design where errors are expensive.

**Difficulty floor**:
A caller-declared minimum difficulty tier below which an automatic assessment cannot place the task.
_Avoid_: Difficulty override when referring only to a minimum

**Explicit tier**:
A caller-supplied difficulty tier used instead of remote task assessment.

**Assessment abstention**:
An assessment outcome indicating insufficient information to assign a difficulty tier. It is not a fifth tier or permission to choose a model without an assessment.

**Selection phase**:
The kind of work for which a candidate policy applies, such as planning, execution, or orchestration.

### Candidate policy

**Candidate**:
An exact model choice permitted by the policy for a selection phase and difficulty tier, including its reasoning setting when specified.

**Candidate group**:
A set of models explicitly considered interchangeable for the relevant phase and tier. Groups have a preference order; membership permits quota-based ordering within a group.
_Avoid_: Provider balance group when referring to candidate interchangeability

**Model selection**:
The choice of one suitable candidate after applying exclusions, quota evidence, and candidate preferences. A selection is a recommendation, not a subagent launch or a quota reservation.

**Provider family**:
The provider identity used for review exclusions, represented by the prefix before the slash in a model reference. Distinct provider families do not necessarily mean distinct underlying model architectures.
_Avoid_: Model lineage

### Quota evidence

**Provider mapping**:
An explicitly configured owner of concrete models and their provider quota evidence. Ownership is distinct from the provider-prefix family used for review exclusions; an unregistered model is not an uncertain-quota candidate.

**Quota headroom**:
The minimum remaining fraction across a provider's usable quota windows. It expresses relative remaining capacity, not comparable token counts or monetary value across providers.

**Confirmed selection**:
A model selection supported by fresh, usable provider quota evidence and no applicable exclusion. It describes observed eligibility, not a guarantee of capacity at execution time.
_Avoid_: Guaranteed under quota

**Uncertain fallback**:
A suitable model selected without confirmed fresh quota evidence when no confirmed candidate qualifies. Missing evidence does not override known disabled, unavailable, or exhausted states.
_Avoid_: Confirmed selection, available capacity

**Dispatch wave**:
A caller-coordinated batch of subagent launches sharing a quota refresh. Its selections do not reserve or divide the observed capacity.
