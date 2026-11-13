// packages/site-sdk/src/header.js
function elementsWithin(root, selector) {
  return [...root.matches?.(selector) ? [root] : [], ...root.querySelectorAll(selector)];
}
function icon(document, name) {
  const node = document.createElement("i");
  node.className = `ph ph-${name}`;
  node.setAttribute("aria-hidden", "true");
  return node;
}
function createHeaderEnhancer(document, site) {
  const window = document.defaultView;
  const records = /* @__PURE__ */ new WeakMap();
  let sequence = 0;
  let opened;
  function uniqueId(kind) {
    let id;
    do {
      id = `idy-${kind}-${++sequence}`;
    } while (document.getElementById(id));
    return id;
  }
  function close(restoreFocus = false) {
    if (!opened) return;
    const menu = opened;
    opened = void 0;
    menu.panel.hidden = true;
    menu.trigger.setAttribute("aria-expanded", "false");
    if (restoreFocus) menu.trigger.focus();
  }
  function place(panel) {
    panel.style.transform = "";
    const bounds = panel.getBoundingClientRect();
    if (bounds.left < 12) panel.style.transform = `translateX(${12 - bounds.left}px)`;
    else if (bounds.right > window.innerWidth - 12) {
      panel.style.transform = `translateX(${window.innerWidth - 12 - bounds.right}px)`;
    }
  }
  function open(menu) {
    close();
    document.querySelectorAll(".compact-header .idy-mobile[open]").forEach((node) => {
      node.open = false;
    });
    opened = menu;
    menu.panel.hidden = false;
    menu.trigger.setAttribute("aria-expanded", "true");
    place(menu.panel);
    (menu.items.find((item) => item.value === menu.select.value) || menu.items[0])?.button.focus();
  }
  function renderRecord(record) {
    for (const menu of record.menus) {
      const selected = menu.select.options[menu.select.selectedIndex];
      const theme = document.documentElement.dataset.theme;
      menu.trigger.replaceChildren(icon(document, menu.kind === "language" ? "translate" : theme === "dark" ? "moon" : "sun"));
      const label = `${site.t(menu.kind)}: ${selected?.textContent || ""}`;
      menu.trigger.setAttribute("aria-label", label);
      menu.trigger.title = label;
      for (const item of menu.items) {
        item.label.textContent = item.option.textContent;
        item.button.setAttribute("aria-checked", String(menu.select.value === item.value));
      }
    }
  }
  function enhance2(root) {
    for (const header of elementsWithin(root, ".compact-header")) {
      const native = header.querySelector(".idy-preferences");
      const selects = ["theme", "language"].map((kind) => native?.querySelector(`select[data-idy-${kind}]`));
      if (selects.some((select) => !select || !select.options.length)) continue;
      const existing = records.get(header);
      if (existing && existing.native === native && existing.controls.isConnected === native.isConnected && existing.menus.every((menu, index) => menu.select === selects[index]) && existing.controls.parentNode === native.parentNode) {
        renderRecord(existing);
        continue;
      }
      if (existing) {
        if (existing.menus.includes(opened)) close();
        existing.controls.remove();
      }
      const controls = document.createElement("div");
      controls.className = "header-controls";
      const record = { native, controls, menus: [] };
      for (const [index, kind] of ["theme", "language"].entries()) {
        const select = selects[index];
        const wrapper = document.createElement("div");
        wrapper.className = "preference-control";
        const trigger = document.createElement("button");
        trigger.type = "button";
        trigger.className = "icon-button";
        trigger.dataset.preference = kind;
        trigger.setAttribute("aria-haspopup", "menu");
        trigger.setAttribute("aria-expanded", "false");
        trigger.id = uniqueId(`${kind}-trigger`);
        const panel = document.createElement("div");
        panel.className = "preference-panel";
        panel.hidden = true;
        panel.id = uniqueId(`${kind}-menu`);
        panel.setAttribute("role", "menu");
        panel.setAttribute("aria-labelledby", trigger.id);
        trigger.setAttribute("aria-controls", panel.id);
        const menu = { root: wrapper, trigger, panel, select, kind, items: [] };
        for (const option of select.options) {
          const button = document.createElement("button");
          button.type = "button";
          button.className = "preference-option";
          button.setAttribute("role", "menuitemradio");
          button.tabIndex = -1;
          button.dataset.value = option.value;
          if (kind === "theme") button.append(icon(document, option.value === "system" ? "desktop" : option.value === "dark" ? "moon" : "sun"));
          const label = document.createElement("span");
          label.className = "option-label";
          const check = icon(document, "check");
          check.classList.add("option-check");
          button.append(label, check);
          button.addEventListener("click", () => {
            select.value = option.value;
            select.dispatchEvent(new window.Event("change", { bubbles: true }));
            renderRecord(record);
            close(true);
          });
          menu.items.push({ button, label, option, value: option.value });
          panel.append(button);
        }
        trigger.addEventListener("click", () => opened === menu ? close() : open(menu));
        trigger.addEventListener("keydown", (event) => {
          if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
          event.preventDefault();
          open(menu);
          if (event.key === "ArrowUp") menu.items.at(-1).button.focus();
        });
        panel.addEventListener("keydown", (event) => {
          const index2 = menu.items.findIndex((item) => item.button === document.activeElement);
          const length = menu.items.length;
          let next;
          if (event.key === "ArrowDown") next = (index2 + 1) % length;
          if (event.key === "ArrowUp") next = (index2 - 1 + length) % length;
          if (event.key === "Home") next = 0;
          if (event.key === "End") next = length - 1;
          if (next !== void 0) {
            event.preventDefault();
            menu.items[next].button.focus();
          } else if (event.key === "Tab") close(true);
        });
        wrapper.addEventListener("focusout", (event) => {
          if (opened === menu && event.relatedTarget && !wrapper.contains(event.relatedTarget)) close();
        });
        wrapper.append(trigger, panel);
        controls.append(wrapper);
        record.menus.push(menu);
      }
      native.after(controls);
      native.hidden = true;
      records.set(header, record);
      renderRecord(record);
    }
  }
  document.addEventListener("pointerdown", (event) => {
    if (opened && !opened.root.contains(event.target)) close();
    document.querySelectorAll(".compact-header .idy-mobile[open]").forEach((mobile) => {
      if (!mobile.contains(event.target)) mobile.open = false;
    });
  });
  document.addEventListener("focusin", (event) => {
    if (opened && !opened.root.contains(event.target)) close();
    document.querySelectorAll(".compact-header .idy-mobile[open]").forEach((mobile) => {
      if (!mobile.contains(event.target)) mobile.open = false;
    });
  });
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") return;
    if (opened) {
      event.preventDefault();
      close(true);
      return;
    }
    const mobile = document.querySelector(".compact-header .idy-mobile[open]");
    if (mobile) {
      event.preventDefault();
      mobile.open = false;
      mobile.querySelector("summary")?.focus();
    }
  });
  document.addEventListener("toggle", (event) => {
    if (event.target.matches?.(".compact-header .idy-mobile") && event.target.open) close();
  }, true);
  document.addEventListener("click", (event) => {
    const link2 = event.target.closest?.(".compact-header .idy-mobile a");
    if (link2) link2.closest(".idy-mobile").open = false;
  });
  window.addEventListener("blur", () => close());
  window.addEventListener("resize", () => {
    if (opened) place(opened.panel);
  });
  return {
    enhance: enhance2,
    render(root = document) {
      for (const header of elementsWithin(root, ".compact-header")) {
        const record = records.get(header);
        if (record) renderRecord(record);
      }
    }
  };
}

// packages/site-sdk/src/runtime.js
var stateKey = Symbol.for("idy.site-sdk.document-state.v1");
var defaults = {
  en: {
    language: "Language",
    theme: "Theme",
    automatic: "Auto",
    system: "System",
    light: "Light",
    dark: "Dark",
    source: "Source",
    releases: "Releases",
    allProjects: "All projects",
    footerNote: "Independent tools. Open source.",
    skipContent: "Skip to content",
    navigation: "Main navigation",
    projectLinks: "Project links",
    loadingRelease: "Checking the latest release\u2026",
    releaseUnavailable: "Release information is unavailable. Browse GitHub Releases for downloads.",
    stableRelease: "Stable release",
    previewRelease: "Preview release",
    viewRelease: "View release",
    copy: "Copy",
    copyCode: "Copy code",
    copied: "Copied.",
    copySelected: "Code selected. Copy it manually.",
    copyManual: "Select the code and copy it manually."
  },
  zh: {
    language: "\u8BED\u8A00",
    theme: "\u4E3B\u9898",
    automatic: "\u81EA\u52A8",
    system: "\u8DDF\u968F\u7CFB\u7EDF",
    light: "\u6D45\u8272",
    dark: "\u6DF1\u8272",
    source: "\u6E90\u4EE3\u7801",
    releases: "\u53D1\u5E03\u7248\u672C",
    allProjects: "\u6240\u6709\u9879\u76EE",
    footerNote: "\u72EC\u7ACB\u5F00\u53D1\uFF0C\u5F00\u653E\u6E90\u4EE3\u7801\u3002",
    skipContent: "\u8DF3\u8F6C\u5230\u6B63\u6587",
    navigation: "\u4E3B\u5BFC\u822A",
    projectLinks: "\u9879\u76EE\u94FE\u63A5",
    loadingRelease: "\u6B63\u5728\u83B7\u53D6\u6700\u65B0\u7248\u672C\u2026",
    releaseUnavailable: "\u6682\u65F6\u65E0\u6CD5\u83B7\u53D6\u7248\u672C\u4FE1\u606F\uFF0C\u8BF7\u524D\u5F80 GitHub Releases \u4E0B\u8F7D\u3002",
    stableRelease: "\u7A33\u5B9A\u7248",
    previewRelease: "\u9884\u53D1\u5E03\u7248",
    viewRelease: "\u67E5\u770B\u53D1\u5E03\u7248\u672C",
    copy: "\u590D\u5236",
    copyCode: "\u590D\u5236\u4EE3\u7801",
    copied: "\u5DF2\u590D\u5236\u3002",
    copySelected: "\u5DF2\u9009\u4E2D\u4EE3\u7801\uFF0C\u8BF7\u624B\u52A8\u590D\u5236\u3002",
    copyManual: "\u8BF7\u9009\u4E2D\u4EE3\u7801\u5E76\u624B\u52A8\u590D\u5236\u3002"
  }
};
function currentDocument() {
  const document = globalThis.document || globalThis.window?.document;
  if (!document?.defaultView) throw new Error("idy.dev site SDK requires a browser document. Importing the SDK is safe during SSR; call initSite after mounting.");
  return document;
}
function normalizedTheme(value) {
  return value === "light" || value === "dark" ? value : "system";
}
function normalizedLanguage(value) {
  return value === "en" || value === "zh" ? value : "auto";
}
function createSite(document) {
  const window = document.defaultView;
  const root = document.documentElement;
  const media = window.matchMedia?.("(prefers-color-scheme: dark)");
  const read = (key, fallback) => {
    try {
      return window.localStorage.getItem(key) || fallback;
    } catch {
      return fallback;
    }
  };
  const save = (key, value) => {
    try {
      window.localStorage.setItem(key, value);
    } catch {
    }
  };
  let themePreference = normalizedTheme(read("idy-theme", "system"));
  let languagePreference = normalizedLanguage(read("idy-language", "auto"));
  const dictionaries = {
    en: Object.assign(/* @__PURE__ */ Object.create(null), defaults.en),
    zh: Object.assign(/* @__PURE__ */ Object.create(null), defaults.zh)
  };
  const copyButtons = /* @__PURE__ */ new WeakSet();
  const copying = /* @__PURE__ */ new WeakSet();
  let headers;
  function browserLanguage() {
    for (const locale of window.navigator.languages || [window.navigator.language || "en"]) {
      const primary = locale.toLowerCase().split(/[-_]/)[0];
      if (primary === "zh" || primary === "en") return primary;
    }
    return "en";
  }
  let language = languagePreference === "auto" ? browserLanguage() : languagePreference;
  const translatedString = (key) => dictionaries[language][key] ?? dictionaries.en[key];
  const t = (key) => translatedString(key) ?? key;
  function syncPreferences(scope) {
    elementsWithin(scope, "select[data-idy-language]").forEach((select) => {
      select.value = languagePreference;
    });
    elementsWithin(scope, "select[data-idy-theme]").forEach((select) => {
      select.value = themePreference;
    });
  }
  function applyTheme() {
    const theme = themePreference === "system" ? media?.matches ? "dark" : "light" : themePreference;
    root.dataset.theme = theme;
    root.style.colorScheme = theme;
    const themeColor = document.querySelector('meta[name="theme-color"]');
    const pageColor = themeColor?.getAttribute(`data-idy-theme-color-${theme}`)?.trim();
    themeColor?.setAttribute("content", pageColor || (theme === "dark" ? "#0b3d73" : "#0b5cad"));
    syncPreferences(document);
    headers?.render();
  }
  function translate(scope = document) {
    root.lang = language === "zh" ? "zh-CN" : "en";
    for (const node of elementsWithin(scope, "[data-i18n]")) {
      const value = translatedString(node.dataset.i18n);
      if (value !== void 0) node.textContent = value;
    }
    for (const node of elementsWithin(scope, "[data-i18n-html]")) {
      const value = translatedString(node.dataset.i18nHtml);
      if (value !== void 0) node.innerHTML = value;
    }
    for (const attribute of ["aria-label", "title", "alt"]) {
      for (const node of elementsWithin(scope, `[data-i18n-${attribute}]`)) {
        const value = translatedString(node.getAttribute(`data-i18n-${attribute}`));
        if (value !== void 0) node.setAttribute(attribute, value);
      }
    }
    if (dictionaries[language].pageTitle) {
      document.title = t("pageTitle");
      document.querySelector('meta[property="og:title"]')?.setAttribute("content", t("pageTitle"));
    }
    if (dictionaries[language].pageDescription) {
      for (const selector of ['meta[name="description"]', 'meta[property="og:description"]']) {
        document.querySelector(selector)?.setAttribute("content", t("pageDescription"));
      }
    }
    syncPreferences(scope);
    renderCopy(scope);
    headers?.render(scope);
  }
  function notifyLanguage() {
    document.dispatchEvent(new window.CustomEvent("idy:languagechange", { detail: { language } }));
  }
  function renderCopy(scope) {
    for (const button of elementsWithin(scope, "button[data-idy-copy]")) {
      if (!copyButtons.has(button)) continue;
      const label = button.querySelector("[data-idy-copy-label]");
      if (label) label.textContent = t(label.dataset.i18n || "copy");
      else if (!button.children.length) button.textContent = t(button.dataset.i18n || "copy");
      button.setAttribute("aria-label", t(button.getAttribute("data-i18n-aria-label") || "copyCode"));
    }
    for (const status of elementsWithin(scope, "[data-idy-copy-status][data-idy-copy-message]")) {
      status.textContent = t(status.dataset.idyCopyMessage);
    }
  }
  function enhanceCopy(scope) {
    for (const button of elementsWithin(scope, "button[data-idy-copy]")) {
      const block = button.closest(".idy-code");
      if (!block?.querySelector("code")) continue;
      button.type = "button";
      copyButtons.add(button);
      let status = block.querySelector("[data-idy-copy-status]");
      if (!status) {
        status = document.createElement("p");
        status.className = "copy-status";
        status.setAttribute("data-idy-copy-status", "");
        block.append(status);
      }
      status.setAttribute("role", "status");
      status.setAttribute("aria-live", "polite");
      status.setAttribute("aria-atomic", "true");
      button.hidden = false;
    }
    renderCopy(scope);
  }
  async function copy(event) {
    const button = event.target.closest?.("button[data-idy-copy]");
    if (!button || !copyButtons.has(button) || button.disabled || copying.has(button)) return;
    const block = button.closest(".idy-code");
    const code = block?.querySelector("code");
    const status = block?.querySelector("[data-idy-copy-status]");
    if (!code || !status) return;
    copying.add(button);
    button.setAttribute("aria-busy", "true");
    let message = "copied";
    try {
      if (!window.navigator.clipboard?.writeText) throw new Error("Clipboard unavailable");
      await window.navigator.clipboard.writeText(code.textContent || "");
    } catch {
      message = "copyManual";
      try {
        const selection = window.getSelection();
        if (selection) {
          const range = document.createRange();
          range.selectNodeContents(code);
          selection.removeAllRanges();
          selection.addRange(range);
          message = "copySelected";
        }
      } catch {
      }
    } finally {
      copying.delete(button);
      button.removeAttribute("aria-busy");
    }
    status.dataset.idyCopyMessage = message;
    status.textContent = t(message);
  }
  const site = {
    get language() {
      return language;
    },
    get theme() {
      return root.dataset.theme;
    },
    t,
    loadRelease,
    asset,
    init(translations = {}) {
      let changed = false;
      for (const locale of ["en", "zh"]) {
        for (const [key, value] of Object.entries(translations?.[locale] || {})) {
          if (typeof value === "string" && dictionaries[locale][key] !== value) {
            dictionaries[locale][key] = value;
            changed = true;
          }
        }
      }
      site.enhance();
      if (changed) notifyLanguage();
      return site;
    },
    setTheme(preference) {
      themePreference = normalizedTheme(preference);
      save("idy-theme", themePreference);
      applyTheme();
    },
    setLanguage(preference) {
      languagePreference = normalizedLanguage(preference);
      save("idy-language", languagePreference);
      language = languagePreference === "auto" ? browserLanguage() : languagePreference;
      translate();
      notifyLanguage();
    },
    enhance(scope = document) {
      if (scope !== document && scope.ownerDocument !== document) throw new TypeError("Enhancement root must belong to this document");
      translate(scope);
      headers.enhance(scope);
      enhanceCopy(scope);
      if (document.readyState !== "loading") delete root.dataset.idyHeaderPending;
      return site;
    }
  };
  document.addEventListener("change", (event) => {
    const select = event.target;
    if (select.matches?.("select[data-idy-language]")) site.setLanguage(select.value);
    if (select.matches?.("select[data-idy-theme]")) site.setTheme(select.value);
  });
  document.addEventListener("click", copy);
  headers = createHeaderEnhancer(document, site);
  if (media?.addEventListener) media.addEventListener("change", applyTheme);
  else media?.addListener?.(applyTheme);
  window.addEventListener("languagechange", () => {
    if (languagePreference === "auto") {
      language = browserLanguage();
      translate();
      notifyLanguage();
    }
  });
  window.addEventListener("storage", (event) => {
    if (event.storageArea) {
      try {
        if (event.storageArea !== window.localStorage) return;
      } catch {
        return;
      }
    }
    if (event.key === "idy-theme" || event.key === null) {
      themePreference = normalizedTheme(event.key === null ? read("idy-theme", "system") : event.newValue);
      applyTheme();
    }
    if (event.key === "idy-language" || event.key === null) {
      languagePreference = normalizedLanguage(event.key === null ? read("idy-language", "auto") : event.newValue);
      language = languagePreference === "auto" ? browserLanguage() : languagePreference;
      translate();
      notifyLanguage();
    }
  });
  if (document.readyState === "loading") {
    root.dataset.idyHeaderPending = "true";
    document.addEventListener("DOMContentLoaded", () => {
      applyTheme();
      site.enhance();
      delete root.dataset.idyHeaderPending;
      notifyLanguage();
    }, { once: true });
    window.addEventListener("load", () => {
      delete root.dataset.idyHeaderPending;
    }, { once: true });
  }
  root.lang = language === "zh" ? "zh-CN" : "en";
  applyTheme();
  return site;
}
function siteFor(document) {
  if (!document[stateKey]) {
    Object.defineProperty(document, stateKey, { value: createSite(document) });
  }
  const site = document[stateKey];
  document.defaultView.IdySite = site;
  return site;
}
function initSite(translations = {}) {
  return siteFor(currentDocument()).init(translations);
}
function enhance(root = currentDocument()) {
  const document = root.nodeType === 9 ? root : root.ownerDocument;
  if (!document?.defaultView) throw new TypeError("Enhancement requires a browser document, element, or document fragment");
  return siteFor(document).enhance(root);
}
function setTheme(preference) {
  siteFor(currentDocument()).setTheme(preference);
}
function setLanguage(preference) {
  siteFor(currentDocument()).setLanguage(preference);
}
function validRepository(repository) {
  return typeof repository === "string" && /^[\w.-]+\/[\w.-]+$/.test(repository) && repository.split("/").every((part) => part !== "." && part !== "..");
}
function validRelease(release, repository) {
  return release && !release.draft && typeof release.tag_name === "string" && release.tag_name.length > 0 && Array.isArray(release.assets) && typeof release.html_url === "string" && release.html_url.startsWith(`https://github.com/${repository}/releases/tag/`) && release.html_url.length > `https://github.com/${repository}/releases/tag/`.length;
}
async function loadRelease(repository) {
  if (!validRepository(repository)) throw new Error("Invalid repository");
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), 8e3);
  const request = (path) => fetch(`https://api.github.com/repos/${repository}/${path}`, {
    headers: { Accept: "application/vnd.github+json" },
    credentials: "omit",
    signal: controller.signal
  });
  try {
    let response = await request("releases/latest");
    if (response.status === 404) {
      response = await request("releases?per_page=100");
      if (!response.ok) throw new Error(`Releases: ${response.status}`);
      const releases = await response.json();
      if (!Array.isArray(releases)) throw new Error("Invalid release list");
      const published = releases.filter((release3) => validRelease(release3, repository)).filter((release3) => Number.isFinite(Date.parse(release3.published_at))).sort((a, b) => Date.parse(b.published_at) - Date.parse(a.published_at));
      const release2 = published.find((item) => !item.prerelease) || published[0];
      if (!release2) throw new Error("No published releases");
      return release2;
    }
    if (!response.ok) throw new Error(`Releases: ${response.status}`);
    const release = await response.json();
    if (!validRelease(release, repository)) throw new Error("Invalid release");
    return release;
  } finally {
    clearTimeout(timeout);
  }
}
function asset(release, pattern) {
  if (!release || typeof release.html_url !== "string" || typeof release.tag_name !== "string" || !release.tag_name || !Array.isArray(release.assets) || !pattern || typeof pattern.test !== "function") return void 0;
  const match = /^https:\/\/github\.com\/([\w.-]+\/[\w.-]+)\/releases\/tag\/.+/.exec(release.html_url);
  if (!match || !validRepository(match[1])) return void 0;
  const encodeSegment = (value) => {
    if (value === "." || value === "..") return void 0;
    try {
      return encodeURIComponent(value);
    } catch {
      return void 0;
    }
  };
  const tag = encodeSegment(release.tag_name);
  if (tag === void 0) return void 0;
  const prefix = `https://github.com/${match[1]}/releases/download/${tag}/`;
  return release.assets.find((item) => {
    pattern.lastIndex = 0;
    if (!item || item.state !== "uploaded" || !Number.isFinite(item.size) || item.size <= 0 || typeof item.name !== "string" || !item.name || !pattern.test(item.name)) return false;
    const name = encodeSegment(item.name);
    return name !== void 0 && item.browser_download_url === prefix + name;
  });
}

// packages/site-sdk/src/components.js
function ownerDocument(options) {
  const document = options.document || globalThis.document || globalThis.window?.document;
  if (!document?.createElement) throw new Error("Creating site components requires a document; pass options.document or call after mounting.");
  return document;
}
function element(document, tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== void 0) node.textContent = text;
  return node;
}
function translated(node, key, fallback) {
  node.textContent = fallback;
  if (key) node.dataset.i18n = key;
  return node;
}
function safeHref(href, document) {
  if (typeof href !== "string" || !href.trim() || /[\u0000-\u001f\u007f]/.test(href)) {
    throw new TypeError("Link href must be a nonempty HTTP(S), mailto, tel, or relative URL");
  }
  let url;
  try {
    url = new URL(href, document.baseURI === "about:blank" ? "https://idy.dev/" : document.baseURI);
  } catch {
    throw new TypeError(`Invalid link href: ${href}`);
  }
  if (!["https:", "http:", "mailto:", "tel:"].includes(url.protocol) || url.username || url.password) {
    throw new TypeError("Link href must use HTTP(S), mailto, tel, or a relative URL without credentials");
  }
  return href;
}
function icon2(document, name) {
  if (typeof name !== "string" || !/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(name)) throw new TypeError("Invalid icon name");
  const node = element(document, "i", `ph ph-${name}`);
  node.setAttribute("aria-hidden", "true");
  return node;
}
function link(document, options, className = "", action = false) {
  const node = element(document, "a", className);
  node.setAttribute("href", safeHref(options.href, document));
  if (options.newTab) {
    node.target = "_blank";
    node.rel = "noopener noreferrer";
  }
  if (options.ariaLabel !== void 0) node.setAttribute("aria-label", options.ariaLabel);
  if (options.ariaLabelKey) node.setAttribute("data-i18n-aria-label", options.ariaLabelKey);
  if (action) {
    const pill = element(document, "span", "idy-action-pill");
    if (options.icon) pill.append(icon2(document, options.icon));
    pill.append(translated(element(document, "span"), options.i18n, options.label));
    node.append(pill);
  } else translated(node, options.i18n, options.label);
  return node;
}
function navigation(document, links, className, key, label) {
  const nav = element(document, "nav", className);
  nav.setAttribute("aria-label", label);
  nav.setAttribute("data-i18n-aria-label", key);
  for (const item of links) nav.append(link(document, item));
  return nav;
}
function brand(document, options, container) {
  container.append(link(document, { label: options.siteName ?? "idy.dev", href: options.siteHref ?? "https://idy.dev/" }, "idy-home"));
  if (options.projectName !== void 0) {
    container.append(options.projectHref !== void 0 ? link(document, { label: options.projectName, href: options.projectHref }, "idy-project") : element(document, "span", "idy-project", options.projectName));
  }
}
var defaultLinks = [
  { label: "All projects", href: "https://idy.dev/", i18n: "allProjects" },
  { label: "Discord", href: "https://discord.gg/TTyqQnw7yu" },
  { label: "GitHub", href: "https://github.com/idy" }
];
function preferences(document) {
  const wrapper = element(document, "div", "idy-preferences");
  const definitions = [
    ["language", "Language", [["auto", "Auto", "automatic"], ["en", "English"], ["zh", "\u4E2D\u6587"]]],
    ["theme", "Theme", [["system", "System", "system"], ["light", "Light", "light"], ["dark", "Dark", "dark"]]]
  ];
  for (const [kind, labelText, options] of definitions) {
    const label = element(document, "label");
    label.append(translated(element(document, "span"), kind, labelText));
    const select = element(document, "select");
    select.setAttribute(`data-idy-${kind}`, "");
    for (const [value, text, key] of options) {
      const option = translated(element(document, "option"), key, text);
      option.value = value;
      select.append(option);
    }
    label.append(select);
    wrapper.append(label);
  }
  return wrapper;
}
function createHeader(options = {}) {
  const document = ownerDocument(options);
  const header = element(document, "header", "idy-header compact-header");
  const inner = element(document, "div", "idy-header-inner");
  brand(document, options, inner);
  const links = options.navLinks ?? defaultLinks;
  if (links.length) {
    inner.append(navigation(document, links, "idy-nav", "navigation", "Main navigation"));
    const mobile = element(document, "details", "idy-mobile");
    const summary = element(document, "summary");
    summary.setAttribute("aria-label", "Main navigation");
    summary.setAttribute("data-i18n-aria-label", "navigation");
    summary.append(icon2(document, "list"));
    mobile.append(summary, navigation(document, links, "idy-mobile-panel", "navigation", "Main navigation"));
    inner.append(mobile);
  }
  const actions = element(document, "div", "header-actions");
  if (options.preferences !== false) actions.append(preferences(document));
  const action = options.action === void 0 ? {
    label: "follow",
    href: "https://github.com/idy",
    newTab: true,
    ariaLabel: "Follow idy on GitHub (opens in a new tab)",
    icon: "github-logo"
  } : options.action;
  if (action) actions.append(link(document, action, "idy-action", true));
  if (actions.childElementCount) inner.append(actions);
  header.append(inner);
  return header;
}
function createFooter(options = {}) {
  const document = ownerDocument(options);
  const footer = element(document, "footer", "idy-footer");
  const inner = element(document, "div", "idy-footer-inner");
  const identity = element(document, "div");
  brand(document, options, identity);
  const note = element(document, "p");
  translated(note, options.noteKey ?? (options.note === void 0 ? "footerNote" : void 0), options.note ?? "Independent tools. Open source.");
  identity.append(note);
  inner.append(identity);
  const links = options.links ?? defaultLinks;
  if (links.length) inner.append(navigation(document, links, "", "projectLinks", "Project links"));
  footer.append(inner);
  return footer;
}
export {
  asset,
  createFooter,
  createHeader,
  enhance,
  initSite,
  loadRelease,
  setLanguage,
  setTheme
};
