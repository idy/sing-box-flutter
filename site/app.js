'use strict';

(() => {
  const profiles = {
    'wgc-minimal': 'TUN, direct, WGC and basic DNS. A small place to start.',
    'wgc-tailscale': 'WGC, WireGuard, Tailscale, selectors and encrypted DNS.',
    'singbox-full': 'The full supported mobile capability set. Broader features mean a larger core.',
  };
  const profileCode = document.querySelector('#profile-code');
  const panel = document.querySelector('#profile-panel');
  const profileButtons = [...document.querySelectorAll('[data-profile]')];
  const profileCache = new Map([['wgc-minimal', profileCode.textContent]]);
  const source = document.querySelector('#profile-source');
  const error = document.querySelector('#profile-error');
  let currentProfile = 'wgc-minimal';
  let requestNumber = 0;

  function highlight(code) {
    const text = code.textContent;
    const fragment = document.createDocumentFragment();
    text.split('\n').forEach((line, index, lines) => {
      const key = line.match(/^(\s*[a-z_]+:)(.*)$/);
      const span = document.createElement('span');
      if (line.trim().startsWith('#')) {
        span.className = 'token-comment';
        span.textContent = line;
        fragment.append(span);
      } else if (key) {
        span.className = 'token-key';
        span.textContent = key[1];
        fragment.append(span);
        const value = document.createElement('span');
        value.className = 'token-value';
        value.textContent = key[2];
        fragment.append(value);
      } else {
        fragment.append(document.createTextNode(line));
      }
      if (index < lines.length - 1) fragment.append(document.createTextNode('\n'));
    });
    code.replaceChildren(fragment);
  }

  function activate(buttons, chosen) {
    buttons.forEach(button => {
      const selected = button === chosen;
      button.setAttribute('aria-selected', String(selected));
      button.tabIndex = selected ? 0 : -1;
    });
  }

  async function selectProfile(name) {
    const request = ++requestNumber;
    currentProfile = name;
    const chosen = profileButtons.find(button => button.dataset.profile === name);
    activate(profileButtons, chosen);
    panel.setAttribute('aria-labelledby', chosen.id);
    panel.setAttribute('aria-busy', 'true');
    error.hidden = true;
    source.href = `https://github.com/idy/sing-box-flutter/blob/main/example/core-profiles/${name}.yaml`;
    document.querySelector('#copy-profile').disabled = true;
    try {
      if (!profileCache.has(name)) {
        const response = await fetch(`./profiles/${name}.yaml`);
        if (!response.ok) throw new Error('Profile unavailable');
        profileCache.set(name, await response.text());
      }
      if (request !== requestNumber) return;
      profileCode.textContent = profileCache.get(name);
      highlight(profileCode);
      document.querySelector('#profile-description').textContent = profiles[name];
      document.querySelector('.profile-pre').scrollTop = 0;
      document.querySelector('#copy-profile').disabled = false;
    } catch {
      if (request !== requestNumber) return;
      profileCode.textContent = '# Profile unavailable. Try again or open its source.';
      document.querySelector('#profile-description').textContent = 'This example profile could not be loaded.';
      error.hidden = false;
    } finally {
      if (request === requestNumber) panel.removeAttribute('aria-busy');
    }
  }

  function keyboardTabs(buttons) {
    buttons.forEach(button => button.addEventListener('keydown', event => {
      const index = buttons.indexOf(button);
      let next;
      if (event.key === 'ArrowRight') next = buttons[(index + 1) % buttons.length];
      if (event.key === 'ArrowLeft') next = buttons[(index + buttons.length - 1) % buttons.length];
      if (event.key === 'Home') next = buttons[0];
      if (event.key === 'End') next = buttons.at(-1);
      if (next) {
        event.preventDefault();
        next.focus();
        next.click();
      }
    }));
  }

  profileButtons.forEach(button => button.addEventListener('click', () => selectProfile(button.dataset.profile)));
  document.querySelector('#retry-profile').addEventListener('click', () => selectProfile(currentProfile));
  keyboardTabs(profileButtons);
  const targetButtons = [...document.querySelectorAll('[data-target]')];
  const requirements = {
    android: 'Python 3.11+, Git, JDK 17, Android SDK and NDK 28.0.13004108. Flutter packaging also uses NDK 28.2.13676358.',
    apple: 'Python 3.11+, Git, macOS, Xcode with iPhoneOS/simulator SDKs, and CocoaPods. The host needs its own signed Packet Tunnel extension.',
  };
  targetButtons.forEach(button => button.addEventListener('click', () => {
    activate(targetButtons, button);
    document.querySelector('#build-panel').setAttribute('aria-labelledby', button.id);
    document.querySelector('#build-command').textContent = `dart run sing_box_flutter build --target ${button.dataset.target}`;
    document.querySelector('#build-requirements').textContent = requirements[button.dataset.target];
  }));
  keyboardTabs(targetButtons);

  document.querySelectorAll('[data-copy]').forEach(button => {
    let reset;
    const label = button.textContent;
    button.addEventListener('click', async () => {
      const code = document.getElementById(button.dataset.copy);
      try {
        await navigator.clipboard.writeText(code.textContent);
        button.textContent = 'Copied';
        document.querySelector('#copy-status').textContent = `${button.getAttribute('aria-label')}: copied.`;
        clearTimeout(reset);
        reset = setTimeout(() => { button.textContent = label; }, 1800);
      } catch {
        const selection = window.getSelection();
        const range = document.createRange();
        range.selectNodeContents(code);
        selection.removeAllRanges();
        selection.addRange(range);
        document.querySelector('#copy-status').textContent = 'Copy is unavailable. The code is selected; use your browser’s Copy command.';
        button.textContent = 'Select + copy';
      }
    });
  });
  highlight(profileCode);
})();
