---
name: browser-ui
description: Use when a task requires interacting with a web UI or browser to implement or verify a feature (frontend testing, checking rendered output, scraping docs).
---

Use the `claude-in-chrome` MCP tools to open pages, click, fill forms, and read console/network output whenever a task requires exercising a real browser to complete or verify work — for example, checking that a UI change renders correctly, or driving a page to reproduce a bug.

Load the tools first with `ToolSearch("select:mcp__claude-in-chrome__tabs_context_mcp,mcp__claude-in-chrome__navigate,mcp__claude-in-chrome__computer,mcp__claude-in-chrome__read_page,mcp__claude-in-chrome__tabs_create_mcp,mcp__claude-in-chrome__tabs_close_mcp")`, then start with `tabs_context_mcp` before creating a new tab.
