---
# Synthetic facet definition frontmatter (LF).
polytoken:
  model: codex/gpt-5.6-sol
  fallback_models:
    - minime/gemma
    - zai/glm-5.2
description: "A managed subagent"
---

# Agent

This is the prose body. It contains YAML-like text that must never change:

```yaml
polytoken:
  model: minime/gemma
  fallback_models:
    - codex/gpt-5.6-sol
```

And a stray `---` divider that is NOT frontmatter:

---
