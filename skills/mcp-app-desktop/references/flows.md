# Flows (run_flow)

Maestro syntax, desktop subset. A flow is a YAML command list, optionally
preceded by a header document (`name`, `env`, `appId`, `tags`).

```yaml
name: create agent
env:
  AGENT: Vérificateur
---
- launchApp                            # no-op: the session is already attached
- assertVisible: "Votre équipe commence ici."
- tapOn: "Créer un agent"              # text or accessible name, unique, case-insensitive
- tapOn:
    role: textbox
    text: "Nom de l’agent"             # role + text = getByRole(role, { name })
- inputText: ${AGENT}                  # types into the focused element
- tapOn: { id: save }                  # DOM id or data-testid
- tapOn: { css: "form button[type=submit]" }
- tapOn: { label: "Email" }
- tapOn: { ref: "@4" }                 # from the last snapshot
- doubleTapOn: { text: "title" }
- tapOn: { point: "50%,90%" }          # viewport percentages or CSS pixels
- pressKey: cmd+k                      # Enter, Escape, Tab, ArrowDown, shift+Tab…
- eraseText: 3
- scroll                               # half a viewport down (direction: UP)
- scrollUntilVisible: { element: "Paramètres", direction: DOWN, timeout: 20000 }
- waitForAnimationToEnd                # DOM stable (bounded)
- extendedWaitUntil: { visible: "Terminé", timeout: 30000 }    # or notVisible:
- assertNotVisible: "Erreur"
- takeScreenshot: after-create         # PNG in the session artifacts dir
- evalJs: "document.title"
- assertJs: "window.escapeDesktop?.version === '0.2.0'"      # must be truthy
- evalMain: "BrowserWindow.getAllWindows().length"
- assertMain: "app.isReady()"
- repeat: { times: 3, commands: [ { pressKey: ArrowDown } ] }
- runFlow: login.yaml                  # or runFlow: { commands: [...] }
```

Any command accepts `optional: true` and a step label in its mapping form
(`label:`, or `stepLabel:` on tapOn/assertVisible where `label` is a selector). Steps
run in order; the first failure stops the flow and returns the screen and a
screenshot path. Mobile-only commands (back, swipe, setLocation…) are
rejected at parse time with a hint.

Selector keys supported: `text`, `role` (+`text`), `label`, `id`, `css`,
`ref`. Not supported: `index`, state filters (`enabled`, `checked`…) and
relative positions (`below`, `above`…): use a CSS selector or a ref instead.

CI: `mcp-app-desktop run-flow flow.yaml --target ./app --env APP_DATA=$(mktemp -d)`
exits 1 on failure and prints the step table.
