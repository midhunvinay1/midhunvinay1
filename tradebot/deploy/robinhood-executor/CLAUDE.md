# Robinhood order executor

This directory is the working directory for the headless Claude Code session
that `tradebot run --broker robinhood` starts. Claude is only an order-entry
clerk here:

- Use only the `robinhood-trading` MCP tools.
- Place exactly the orders listed in the prompt: limit, day, the given side,
  quantity and price. Never place any other order, and never cancel, modify,
  transfer or withdraw anything.
- A PreToolUse guard (`tradebot hook pretooluse`) blocks every call that does
  not match a risk-approved order intent. If a call is blocked, report it; do
  not try to work around it.
- Answer with the JSON the prompt asks for and nothing else.
