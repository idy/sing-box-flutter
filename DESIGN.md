---
name: sing_box_flutter website
description: A calm developer reading surface in the approved blue/slate identity.
colors:
  blue: "#2454e6"
  blue-dark: "#173ab0"
  ink: "#17243d"
  muted: "#53627a"
  paper: "#fff"
  canvas: "#f7f9fd"
  tint: "#eff3fd"
  line: "#d9e1ef"
  code: "#111e37"
  code-ink: "#dce6f7"
  code-line: "#2b3953"
  code-muted: "#a8bad8"
  tab-muted: "#9fb1cf"
  tab-active: "#84a7ff"
  syntax-key: "#91baff"
  syntax-value: "#a7e6c7"
  syntax-comment: "#9aaecb"
  focus: "#ed9c22"
typography:
  display:
    fontFamily: "Manrope, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif"
    fontSize: "clamp(3.5rem, 5.5vw, 5.15rem)"
    fontWeight: 800
    lineHeight: 1.06
    letterSpacing: "-0.04em"
  headline:
    fontFamily: "Manrope, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif"
    fontSize: "clamp(2rem, 3.5vw, 3rem)"
    fontWeight: 750
    lineHeight: 1.14
    letterSpacing: "-0.035em"
  title:
    fontFamily: "Manrope, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif"
    fontSize: "1.25rem"
    fontWeight: 750
    lineHeight: 1.14
    letterSpacing: "-0.035em"
  lead:
    fontSize: "22px"
    fontWeight: 600
    lineHeight: 1.45
    letterSpacing: "-0.02em"
  body:
    fontFamily: "Manrope, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif"
    fontSize: "16px"
    fontWeight: 400
    lineHeight: 1.7
  reading:
    fontSize: "14px"
    lineHeight: 1.85
  label:
    fontSize: "12px"
  action:
    fontSize: "14px"
    fontWeight: 700
  code:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"
    fontSize: "14px"
    lineHeight: 1.9
rounded:
  badge: "4px"
  control: "5px"
  button: "7px"
  code-block: "12px"
  recipe: "14px"
spacing:
  inline: "8px"
  compact: "12px"
  control: "18px"
  code-inset: "22px"
  group: "30px"
  row: "34px"
  section-gap: "42px"
components:
  button-primary:
    backgroundColor: "{colors.blue}"
    textColor: "{colors.paper}"
    typography: "{typography.action}"
    rounded: "{rounded.button}"
    padding: "10px 18px"
  button-primary-hover:
    backgroundColor: "{colors.blue-dark}"
    textColor: "{colors.paper}"
  copy-button:
    backgroundColor: "transparent"
    textColor: "#c5d6f5"
    rounded: "{rounded.control}"
    padding: "5px 9px"
  profile-tab:
    backgroundColor: "transparent"
    textColor: "{colors.tab-muted}"
    padding: "15px 0 12px"
  profile-tab-selected:
    textColor: "{colors.paper}"
  recipe:
    backgroundColor: "{colors.code}"
    textColor: "{colors.code-ink}"
    rounded: "{rounded.recipe}"
  code-block:
    backgroundColor: "{colors.code}"
    textColor: "{colors.code-ink}"
    rounded: "{rounded.code-block}"
  measurement-badge:
    backgroundColor: "#d7e2ff"
    textColor: "#173fbe"
    rounded: "{rounded.badge}"
    padding: "3px 5px"
---

# Design System: sing_box_flutter website

## Overview

**Creative North Star: "The readable build recipe"**

This website extends the approved `doc/images/overview.svg` blue/slate identity in Read mode. Cool white space, strong left-aligned headings and working code examples make a calm developer document. The metaphor describes the finished surface rather than a new identity.

Open comparisons and ruled instruction rows carry the reading flow. Dark code panels contain the technical material; inline SVG supplies the brand and directional marks. The shipped site uses no raster art.

**Key Characteristics:**

- Blue actions and slate reading text.
- Open ruled sections with dark technical panels.
- Self-hosted Manrope with monospace reserved for code.
- Visible content and restrained state feedback.

Evidence: `site/style.css`, `site/index.html`, `site/app.js`, built `.build/site/index.html`, and the desktop/mobile captures in `.impeccable/review/`. This system describes the website, not the native Flutter interface.

## Colors

### Primary

Blue marks the primary action, emphasized headline text and build output; blue-dark is its hover state. Pale blue selected-tab lines distinguish selection on dark panels. Syntax keys use blue and values use mint; these are code semantics rather than extra brand accents.

### Neutral

Ink and muted slate establish the reading hierarchy on paper. Tint creates a continuous comparison section; line provides page dividers. Code and code-ink provide the dark technical surface, with code-line separators and muted supporting text. Canvas is the stylesheet's scrollbar-track color; the page itself uses paper.

**The Blue Action Rule.** Use blue for the principal action and meaningful emphasis; keep explanatory prose in slate.

## Typography

Manrope is self-hosted as a variable WOFF2 face covering weights 400–800, with `font-display: swap` and the system sans fallback stack recorded above. Use the monospace stack only for commands, configuration and inline code.

The frontmatter records the desktop hierarchy. Display is the hero heading, headline is section-level text, and title is subsection text; instruction titles use a more open line height (1.4). Headings are balanced and tightly tracked. Lead text introduces the explanation, reading text carries most supporting paragraphs, and labels carry metadata. Navigation and actions use the action size; navigation weight is 700. Ordinary metadata is mostly 12px, while tab and code-block text is 13px.

Hero description is 15px and limited to 43ch on desktop, 48ch on mobile. Section introductions use approximately 36ch, expanding to 54ch on mobile. These are contextual reading widths, not a universal paragraph limit.

**The Reading and Code Rule.** Keep prose in Manrope and executable or configuration text in monospace.

## Layout

A continuous centered container has a maximum width of 1184px and desktop side gutters of 48px. The desktop hero uses columns of 1fr and 1.08fr with a 72px gap. Comparisons and instruction rows share a narrower prose column and wider technical column (.85fr / 1.3fr, 100px gap). Advantages use three open columns, separated from the comparison by a horizontal rule.

Spacing is contextual rather than a strict numeric scale. Repeated control and code insets are recorded in frontmatter; larger section spacing falls around 60–88px. Hero vertical padding is 82px above and 70px below. Rules, alignment and whitespace group content without enclosing each paragraph in a card.

- At widths of at least 1600px, hero padding expands to 108px / 84px.
- At 1100px and below, gutters become 32px, hero gap becomes 36px, and comparison/instruction columns become .9fr / 1.2fr with a 50px gap. Display becomes `clamp(3.2rem, 6vw, 4.5rem)`.
- At 760px and below, gutters become 20px. Hero, comparison, advantages and instruction rows stack; the hero is capped at 580px. Display becomes `clamp(3rem, 10vw, 4.1rem)` with line height 1.08; lead text becomes 20px. Header height reduces from 106px to 80px, and the first navigation link is hidden. Resources and footer stack, build-process labels wrap, and code-block text becomes 12px.
- At 390px and below, brand/navigation/tab text and action insets tighten. Comparison badges move below their values; table values reduce to 17px.

Code scrolls inside its panel instead of widening the page. The profile viewport is 254px high on desktop and 252px on mobile. Preserve readable source order as the columns stack.

## Elevation & Depth

The page is predominantly flat: tonal bands, thin rules and dark code surfaces provide separation. Only the hero recipe has a diffuse shadow (`0 20px 48px -22px #1e36576b`). Ordinary code blocks remain unshadowed. No entrance animation is used. The primary action transitions its background over .18s with ease-out; smooth anchor scrolling and transitions are disabled for reduced-motion preferences.

**The Flat Reading Rule.** Use open rules and tonal separation for reading sections; reserve the shipped ambient shadow for the hero recipe.

## Shapes

Code panels have gently rounded corners, with the recipe slightly rounder than instruction blocks. Primary actions, copy controls and measurement badges use progressively smaller radii as recorded above. Step counters and platform separators are circular. Borders are thin and quiet; dark panels use their own darker separator. Directional icons are inline stroked SVG, and the brand uses four blue square forms.

## Components

- **Primary action:** a blue anchor styled as a compact button, minimum height 48px, with a 26px gap to its SVG arrow. Hover darkens the fill. Secondary actions are text links with inline arrows and underlines on hover.
- **Copy control:** a compact outlined control on dark code, minimum height 32px, with a slate border. Hover adds a lighter dark fill. Copy feedback temporarily changes the label to “Copied” (1800ms); a polite live region announces the result. Clipboard failure selects the code and offers manual copying.
- **Navigation:** an open horizontal row of text links, without a surrounding pill. Desktop gap is 30px; mobile retains Quick start and GitHub. The wordmark links to main content.
- **Profile recipe:** a labeled file header, tab row, focusable code panel, copy control and source-note footer. Profile tabs have a 44px minimum height and a 2px blue selected underline. Build-target tabs use an underlined selected label. Both use `aria-selected`, associated tab panels and roving tab stops; Left/Right and Home/End move and activate selection.
- **Loading and failure:** profile changes mark the panel busy and disable copying while loading. A visible failure message offers retry and the source link. The built HTML embeds the default profile and commands so the initial document remains readable without JavaScript.
- **Instruction blocks:** dark, unshadowed code containers paired with numbered prose rows. Preserve preformatted whitespace, horizontal scrolling and compact metadata rather than wrapping commands into ambiguous lines.
- **Comparison and badge:** a semantic table with caption, column/row headers and tabular numerals. Selected measurements are blue and bold; compact percentage badges reinforce the numeric result. Badge placement changes at the documented breakpoints.

Accessibility conventions are visible keyboard focus (3px amber outline, 5px offset), a focus-revealed skip link, named navigation/sections, semantic headings and lists, and hidden decorative SVG. Focusable panels receive the same outline. Preserve reduced-motion behavior and the polite copy-status region. Compact controls are observed dimensions, not a general accessibility target-size recommendation.

## Do's and Don'ts

### Do:

- **Do** carry the approved blue/slate identity through readable prose, meaningful blue emphasis and dark code panels.
- **Do** preserve keyboard focus, tab semantics, reduced-motion behavior and readable default HTML.
- **Do** use open rules and shared column alignment to organize comparisons and instructions.

### Don't:

- **Don't** replace the self-hosted reading face with a system display face or use monospace for prose.
- **Don't** introduce raster decoration into this SVG-and-code website.
- **Don't** let long code widen the page; keep scrolling inside technical panels.
