/**
 * The rules the SoulMirror surfaces need that inline styles cannot express
 * (hover / focus chrome, the unread dot, the rail circle, bubbles, the typing
 * dots, the three-column page, the draft cards). The client bundle has no
 * CSS-module pipeline (tsdown replica, SPIKE.md §6), so one `<style>` element
 * is appended to the document once. Every colour is a `--dsw-*` token (the
 * ones ui-layout / ui-theme set on the document), so the page follows dsh's
 * theme and looks native — the LAYOUT follows the SoulMirror prototype
 * (docs/superpowers/specs/2026-08-23-dsh-soulmirror-chat-alignment.html, #B),
 * the COLOURS and type follow dsh.
 */
const STYLE_ID = 'soulmirror-styles'

const CSS = `
/* New-mail toast: our own pill (the host Toast surface stays light on the
   dark theme). Fixed top-center, themed, auto-dismissing. */
.sm-mail-toast {
  position: fixed; top: 14px; left: 50%; transform: translateX(-50%); z-index: 90;
  display: flex; align-items: center; gap: 8px; max-width: min(420px, 80vw);
  padding: 8px 14px; border: 1px solid var(--dsw-alias-border-l2); border-radius: 999px;
  background: var(--dsw-specific-menu, var(--dsw-alias-bg-layer-2)); color: var(--dsw-alias-label-primary);
  box-shadow: var(--dsw-shadow-lv3, 0 8px 24px rgba(0,0,0,.35)); cursor: pointer;
  font: inherit; font-size: 12px; animation: sm-page-in 140ms ease-out;
}
.sm-mail-toast:hover { border-color: var(--dsw-alias-brand-primary); }

/* Icon-only update button at the right end of the SoulMirror foot row:
   invisible until a release is known, small circled arrow, tooltip carries
   the words. Sits beside the entry (the row is a flex line). */
.sm-update-fab {
  flex: none; align-self: center; width: 24px; height: 24px; margin: 4px 2px 0 0;
  display: inline-flex; align-items: center; justify-content: center;
  border: 1px solid var(--dsw-alias-brand-primary); border-radius: 50%;
  background: transparent; color: var(--dsw-alias-brand-primary); cursor: pointer; padding: 0;
}
.sm-update-fab:hover { background: var(--dsw-alias-brand-primary); color: var(--dsw-alias-label-primary-inverted); }
.sm-update-fab:disabled, .sm-update-fab.sm-busy { opacity: .8; cursor: default; }
.sm-update-spin { animation: sm-update-spin 900ms linear infinite; }
@keyframes sm-update-spin { to { transform: rotate(360deg); } }
  display: inline-flex; align-items: center; gap: 5px; max-width: 100%; min-width: 0;
  padding: 2px 9px; border: 1px solid var(--dsw-alias-brand-primary); border-radius: 999px;
  background: transparent; color: var(--dsw-alias-brand-primary);
  font: inherit; font-size: 11px; font-weight: 600; cursor: pointer; white-space: nowrap;
}
.sm-update-chip:hover { background: var(--dsw-alias-brand-primary); color: var(--dsw-alias-label-primary-inverted); }
.sm-update-chip:disabled { opacity: .75; cursor: default; }
.sm-update-chip.sm-rail { padding: 2px 6px; }


/* Native form controls follow the ACTIVE theme: the option list of a <select>
   (and scrollbars, checkboxes...) is OS-rendered and ignores CSS tokens - it
   only obeys color-scheme. dsh marks its dark theme on <body>. */
body[data-ds-dark-theme] { color-scheme: dark; }
.sm-page-root select option, .sm-page-root select optgroup { background: var(--dsw-specific-menu); color: var(--dsw-alias-label-primary); }

.sm-footer {
  flex: 1 1 auto; min-width: 0; display: flex; align-items: center; gap: 8px;
  width: auto; height: 42px; margin: 4px -2px 0; padding: 0 10px 0 8px;
  box-sizing: border-box; border: none; border-radius: 12px; background: transparent;
  cursor: pointer; overflow: hidden; color: var(--dsw-alias-label-primary);
  font-family: inherit; font-size: 14px; line-height: 22px; position: relative;
}
.sm-footer:hover, .sm-footer[aria-expanded="true"] { background: var(--dsw-alias-interactive-bg-hover); }
.sm-footer.sm-rail {
  width: 36px; height: 36px; margin: 8px 0 0; padding: 0; gap: 0;
  justify-content: center; border-radius: 50%;
}
.sm-nav {
  flex: none; display: flex; align-items: center; gap: 8px;
  width: 100%; height: 36px; margin: 0; padding: 0 10px 0 8px;
  box-sizing: border-box; border: none; border-radius: 10px; background: transparent;
  cursor: pointer; overflow: hidden; color: var(--dsw-alias-label-primary);
  font-family: inherit; font-size: 14px; line-height: 22px; position: relative;
}
.sm-nav:hover, .sm-nav.sm-nav-active { background: var(--dsw-alias-interactive-bg-hover); }
.sm-nav.sm-rail {
  width: 36px; height: 36px; padding: 0; gap: 0;
  justify-content: center; border-radius: 50%;
}
.sm-footer-label { flex: 1; min-width: 0; overflow: hidden; white-space: nowrap; text-align: left; }
.sm-footer-icon { flex: none; display: inline-flex; position: relative; }
.sm-badge {
  flex: none; display: inline-flex; align-items: center; justify-content: center;
  min-width: 18px; height: 18px; padding: 0 5px; box-sizing: border-box; border-radius: 9px;
  background: var(--dsw-alias-brand-primary); color: var(--dsw-alias-label-primary-inverted);
  font-size: 11px; font-weight: 600; line-height: 18px;
}
.sm-badge.sm-badge-warn { background: var(--dsw-alias-state-warn-primary); color: var(--dsw-alias-label-primary-inverted); white-space: nowrap; }
.sm-dot {
  position: absolute; top: -2px; right: -3px; width: 9px; height: 9px; border-radius: 50%;
  background: var(--dsw-alias-state-error-primary); box-shadow: 0 0 0 2px var(--dsw-specific-sidebar-fill);
}
.sm-row {
  display: flex; align-items: center; gap: 10px; width: 100%; padding: 8px 10px; box-sizing: border-box;
  border: none; border-radius: 10px; background: transparent; color: inherit; font: inherit; text-align: left; cursor: pointer;
  position: relative;
}
.sm-row:hover, .sm-row:focus-visible { background: var(--dsw-alias-interactive-bg-hover); outline: none; }
.sm-row.sm-selected, .sm-row.sm-selected:hover { background: var(--dsw-alias-interactive-bg-hover-solid, var(--dsw-alias-interactive-bg-hover)); }
.sm-row-body { flex: 1; min-width: 0; display: grid; gap: 1px; }
.sm-row-title { display: flex; align-items: baseline; gap: 6px; min-width: 0; }
.sm-row-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 500; }
.sm-row-time { flex: none; font-size: 11px; color: var(--dsw-alias-label-tertiary); }
.sm-row-preview { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; color: var(--dsw-alias-label-secondary); }
.sm-row-preview.sm-unread { color: var(--dsw-alias-label-primary); }
.sm-row-preview .sm-alert { color: var(--dsw-alias-state-warn-primary); font-weight: 600; }
.sm-row.sm-row-alter::before {
  content: ""; position: absolute; left: 0; top: 6px; bottom: 6px; width: 3px; border-radius: 2px;
  background: var(--dsw-alias-brand-primary);
}
.sm-avawrap { position: relative; flex: none; }
.sm-avatar {
  width: 34px; height: 34px; border-radius: 9px; display: flex; align-items: center; justify-content: center;
  background: var(--dsw-alias-interactive-bg-hover-solid, var(--dsw-alias-bg-layer-2)); color: var(--dsw-alias-label-primary);
  font-size: 14px; font-weight: 600; flex: none; user-select: none;
}
.sm-avatar.sm-avatar-alter { background: var(--dsw-alias-brand-primary); color: var(--dsw-alias-label-primary-inverted); }
.sm-avatar.sm-avatar-lg { width: 36px; height: 36px; font-size: 16px; border-radius: 11px; }
.sm-avatar.sm-avatar-sm { width: 26px; height: 26px; font-size: 12px; border-radius: 7px; }
.sm-avawrap .sm-presence { position: absolute; right: -2px; bottom: -2px; width: 10px; height: 10px; border: 2px solid var(--dsw-specific-sidebar-fill, var(--dsw-alias-bg-base)); opacity: 1; }
.sm-avawrap .sm-gmark {
  position: absolute; right: -4px; bottom: -4px; width: 15px; height: 15px; border-radius: 5px;
  background: var(--dsw-alias-bg-base); border: 1.5px solid var(--dsw-specific-sidebar-fill, var(--dsw-alias-bg-base));
  color: var(--dsw-alias-label-secondary); display: flex; align-items: center; justify-content: center; box-sizing: border-box;
}
.sm-avawrap .sm-badge { position: absolute; top: -6px; right: -8px; border: 2px solid var(--dsw-specific-sidebar-fill, var(--dsw-alias-bg-base)); }
.sm-presence { flex: none; width: 8px; height: 8px; border-radius: 50%; background: var(--dsw-alias-label-tertiary); opacity: .5; }
.sm-presence.sm-online { background: var(--dsw-alias-state-success-primary); opacity: 1; }
.sm-livedot { display: inline-block; width: 6px; height: 6px; border-radius: 50%; background: var(--dsw-alias-state-success-primary); box-shadow: 0 0 0 2px var(--dsw-alias-state-success-tertiary, transparent); vertical-align: middle; }
.sm-livedot.sm-busy { background: var(--dsw-alias-state-warn-primary); box-shadow: 0 0 0 2px var(--dsw-alias-state-warn-tertiary, transparent); }
.sm-iconbtn {
  flex: none; display: inline-flex; align-items: center; justify-content: center; width: 24px; height: 24px;
  border: none; border-radius: 6px; background: transparent; color: var(--dsw-alias-label-secondary); cursor: pointer; padding: 0;
}
.sm-iconbtn:hover { background: var(--dsw-alias-interactive-bg-hover); color: var(--dsw-alias-label-primary); }
.sm-iconbtn:disabled { opacity: .45; cursor: default; }
.sm-input {
  flex: 1; min-width: 0; height: 28px; padding: 0 8px; box-sizing: border-box; font: inherit; font-size: 12px;
  border: 1px solid var(--dsw-alias-border-l2); border-radius: 8px; background: var(--dsw-alias-bg-base); color: inherit;
}
.sm-input:focus { outline: none; border-color: var(--dsw-alias-brand-primary); }
.sm-section { padding: 8px 10px 4px; font-size: 11px; font-weight: 600; letter-spacing: .04em; text-transform: uppercase; color: var(--dsw-alias-label-tertiary); }
.sm-muted { color: var(--dsw-alias-label-secondary); }
.sm-page { max-width: 720px; margin: 0 auto; padding: 16px 20px; font-size: 13px; line-height: 20px; }
.sm-page .sm-row { padding: 10px 12px; }

/* ——— the SoulMirror page: middle column + right pane, right of dsh's sidebar ——— */
.sm-page-root {
  position: absolute; top: 0; bottom: 0; right: 0; z-index: 25;
  display: flex; min-width: 0; overflow: hidden;
  background: var(--dsw-alias-bg-base); color: var(--dsw-alias-label-primary);
  border-left: 1px solid var(--dsw-alias-border-l1); font-size: 13px; line-height: 20px;
  animation: sm-page-in 120ms ease-out;
}
@keyframes sm-page-in { from { opacity: 0; } }
.sm-list-col {
  flex: none; width: 300px; min-width: 240px; display: flex; flex-direction: column; min-height: 0;
  border-right: 1px solid var(--dsw-alias-border-l1); background: var(--dsw-specific-sidebar-fill, transparent);
}
.sm-list-head { flex: none; display: flex; align-items: center; gap: 8px; padding: 12px 12px 8px; }
.sm-list-head-title { display: flex; align-items: center; gap: 8px; min-width: 0; font-weight: 600; font-size: 15px; }
.sm-list-search { flex: none; padding: 0 12px 8px; display: flex; gap: 6px; align-items: center; }
.sm-col2-tabs { flex: none; display: flex; gap: 2px; padding: 0 8px 6px; border-bottom: 1px solid var(--dsw-alias-border-l2); }
.sm-col2-tab { flex: 1; border: 0; background: transparent; color: var(--dsw-alias-label-secondary); font-size: 13px; padding: 6px 4px; border-bottom: 2px solid transparent; cursor: pointer; }
.sm-col2-tab:hover { color: var(--dsw-alias-label-primary); }
.sm-col2-tab.sm-active { color: var(--dsw-alias-brand-primary); border-bottom-color: var(--dsw-alias-brand-primary); font-weight: 600; }
.sm-list-body { flex: 1; min-height: 0; overflow-y: auto; padding: 0 6px 6px; }
.sm-list-foot { flex: none; padding: 8px 12px 12px; border-top: 1px solid var(--dsw-alias-border-l2); display: grid; gap: 6px; }
.sm-req { display: flex; align-items: center; gap: 8px; margin: 2px 4px 4px; padding: 8px 10px; border: 1px solid var(--dsw-alias-border-l2); border-radius: 10px; background: var(--dsw-alias-bg-base); }
.sm-chat-col { flex: 1; min-width: 0; display: flex; flex-direction: column; min-height: 0; background: var(--dsw-alias-bg-base); }
.sm-chat-head {
  flex: none; display: flex; align-items: center; gap: 10px; min-height: 56px; padding: 8px 16px; box-sizing: border-box;
  border-bottom: 1px solid var(--dsw-alias-border-l2);
}
.sm-chat-head-name { font-weight: 600; font-size: 15px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.sm-chat-head-sub { font-size: 12px; color: var(--dsw-alias-label-secondary); display: flex; align-items: center; gap: 6px; min-width: 0; }
.sm-chat-head-actions { flex: none; display: flex; align-items: center; gap: 4px; }
.sm-pane-tabs { flex: none; display: flex; gap: 2px; padding: 0 16px; border-bottom: 1px solid var(--dsw-alias-border-l2); box-sizing: border-box; }
.sm-pane-tab { border: 0; background: transparent; color: var(--dsw-alias-label-secondary); font-size: 13px; padding: 9px 4px; margin-right: 14px; border-bottom: 2px solid transparent; cursor: pointer; }
.sm-pane-tab:hover { color: var(--dsw-alias-label-primary); }
.sm-pane-tab.sm-active { color: var(--dsw-alias-brand-primary); border-bottom-color: var(--dsw-alias-brand-primary); font-weight: 600; }
.sm-banner {
  flex: none; display: flex; align-items: center; gap: 10px; padding: 7px 16px; font-size: 12.5px;
  background: var(--dsw-alias-state-warn-tertiary, var(--dsw-alias-interactive-bg-hover)); color: var(--dsw-alias-state-warn-label, var(--dsw-alias-label-primary));
  border-bottom: 1px solid var(--dsw-alias-border-l2);
}
.sm-banner b { font-weight: 600; }
.sm-banner .sm-ghostbtn { margin-left: auto; background: var(--dsw-alias-bg-base); }
.sm-pendbar {
  flex: none; display: flex; align-items: center; gap: 8px; padding: 7px 16px; font-size: 12.5px;
  background: var(--dsw-alias-state-warn-tertiary, var(--dsw-alias-interactive-bg-hover)); color: var(--dsw-alias-state-warn-label, var(--dsw-alias-label-primary));
  border-bottom: 1px solid var(--dsw-alias-border-l2);
}
.sm-pendbar b { color: var(--dsw-alias-state-warn-primary); }
.sm-pendbar .sm-linkbtn { margin-left: auto; }
.sm-thread { flex: 1; min-height: 0; overflow-y: auto; padding: 12px 20px; display: flex; flex-direction: column; }
.sm-thread-inner { display: flex; flex-direction: column; gap: 4px; }
.sm-day { align-self: center; margin: 10px 0 6px; padding: 1px 10px; border-radius: 10px; font-size: 11px;
  color: var(--dsw-alias-label-tertiary); background: var(--dsw-alias-interactive-bg-hover); }
.sm-msg { display: flex; flex-direction: column; max-width: 72%; }
.sm-msg.sm-in { align-self: flex-start; align-items: flex-start; }
.sm-msg.sm-out { align-self: flex-end; align-items: flex-end; }
.sm-bubble {
  padding: 7px 11px; border-radius: 14px; white-space: pre-wrap; overflow-wrap: anywhere; font-size: 13.5px; line-height: 20px;
}
.sm-in .sm-bubble { background: var(--dsw-specific-bubble, var(--dsw-alias-interactive-bg-hover)); color: var(--dsw-alias-label-primary); border-bottom-left-radius: 4px; }
.sm-out .sm-bubble { background: var(--dsw-alias-brand-primary, var(--dsw-static-deepseek-500)); color: var(--dsw-alias-label-primary-inverted); border-bottom-right-radius: 4px; }
.sm-out .sm-bubble.sm-pending { opacity: .65; }
.sm-out .sm-bubble.sm-failed { background: var(--dsw-alias-state-error-primary); }
.sm-msg-meta { display: flex; align-items: center; gap: 6px; margin: 2px 4px 0; font-size: 11px; color: var(--dsw-alias-label-tertiary); }
.sm-msg-meta .sm-status-failed { color: var(--dsw-alias-state-error-primary); }
.sm-linkbtn { border: none; background: none; padding: 0; font: inherit; font-size: 12px; color: var(--dsw-alias-brand-primary); cursor: pointer; }
.sm-linkbtn:disabled { opacity: .5; cursor: default; }
.sm-typing { display: flex; align-items: center; gap: 6px; padding: 2px 20px 6px; font-size: 12px; color: var(--dsw-alias-label-secondary); min-height: 22px; }
.sm-typing-dots span { display: inline-block; width: 5px; height: 5px; margin-right: 2px; border-radius: 50%; background: currentColor; opacity: .35; animation: sm-blink 1.2s infinite; }
.sm-typing-dots span:nth-child(2) { animation-delay: .2s; }
.sm-typing-dots span:nth-child(3) { animation-delay: .4s; }
@keyframes sm-blink { 0%, 80%, 100% { opacity: .25; } 40% { opacity: 1; } }
.sm-composer { flex: none; padding: 8px 16px 12px; border-top: 1px solid var(--dsw-alias-border-l2); display: grid; gap: 6px; }
.sm-composer-box {
  display: flex; align-items: flex-end; gap: 6px; padding: 6px 6px 6px 12px; box-sizing: border-box;
  border: 1px solid var(--dsw-alias-border-l2); border-radius: 14px; background: var(--dsw-specific-input-major, var(--dsw-alias-bg-layer-1));
}
.sm-composer-box:focus-within { border-color: var(--dsw-alias-brand-primary); }
.sm-textarea {
  flex: 1; min-width: 0; max-height: 160px; min-height: 24px; padding: 2px 0; resize: none; border: none; outline: none;
  background: transparent; color: inherit; font: inherit; font-size: 13.5px; line-height: 20px;
}
.sm-composer-hint { font-size: 11px; color: var(--dsw-alias-label-tertiary); padding: 0 4px; }
.sm-actbar {
  flex: none; display: flex; align-items: center; gap: 6px; flex-wrap: wrap; padding: 10px 16px;
  border-top: 1px solid var(--dsw-alias-border-l2); background: var(--dsw-alias-bg-layer-1, var(--dsw-alias-bg-base));
}
.sm-actbar-eye { font-size: 12px; color: var(--dsw-alias-label-tertiary); margin-right: auto; }
.sm-empty { flex: 1; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 8px; padding: 24px; text-align: center; color: var(--dsw-alias-label-secondary); }
.sm-empty-title { font-size: 15px; font-weight: 600; color: var(--dsw-alias-label-primary); }
.sm-empty p { margin: 0; max-width: 420px; font-size: 12.5px; }
.sm-card-pop {
  position: absolute; right: 16px; top: 60px; z-index: 2; width: min(420px, calc(100% - 32px)); padding: 10px 12px; box-sizing: border-box;
  border: 1px solid var(--dsw-alias-border-l1); border-radius: 12px; background: var(--dsw-alias-bg-layer-2, var(--dsw-alias-bg-base)); box-shadow: var(--dsw-shadow-lv3);
  display: grid; gap: 6px; font-size: 12px;
}
.sm-card-uri { font-family: var(--ds-font-family-code, ui-monospace, monospace); font-size: 11px; overflow-wrap: anywhere; color: var(--dsw-alias-label-secondary); max-height: 96px; overflow: auto; }
.sm-pending-actions { display: flex; gap: 4px; flex: none; }
.sm-ghostbtn {
  display: inline-flex; align-items: center; gap: 4px; height: 26px; padding: 0 9px; border: 1px solid var(--dsw-alias-border-l2); border-radius: 8px;
  background: transparent; color: var(--dsw-alias-label-primary); font: inherit; font-size: 12px; cursor: pointer; white-space: nowrap;
}
.sm-ghostbtn:hover { background: var(--dsw-alias-interactive-bg-hover); }
.sm-ghostbtn:disabled { opacity: .5; cursor: default; }
.sm-ghostbtn.sm-warnbtn { background: var(--dsw-alias-state-warn-tertiary, transparent); border-color: var(--dsw-alias-state-warn-secondary, var(--dsw-alias-border-l2)); color: var(--dsw-alias-state-warn-label, var(--dsw-alias-label-primary)); font-weight: 600; }
.sm-ghostbtn.sm-warnbtn b { color: var(--dsw-alias-state-warn-primary); margin-left: 2px; }
.sm-ghostbtn.sm-dangerbtn { color: var(--dsw-alias-state-error-primary); }
.sm-ghostbtn.sm-dangerbtn:hover { background: var(--dsw-alias-interactive-bg-hover-danger, var(--dsw-alias-interactive-bg-hover)); }

/* ——— drafts: the owner's review card (friend thread + alter chat) ——— */
.sm-draft {
  margin: 8px 0; padding: 10px 14px; box-sizing: border-box; display: grid; gap: 6px; font-size: 12.5px; line-height: 18px;
  border: 1px solid var(--dsw-alias-border-l2); border-left: 3px solid var(--dsw-alias-state-warn-primary); border-radius: 10px;
  background: var(--dsw-alias-state-warn-tertiary, var(--dsw-alias-bg-layer-1)); color: var(--dsw-alias-label-primary); max-width: 560px; align-self: stretch;
}
.sm-draft-head { display: flex; align-items: center; gap: 8px; font-weight: 600; }
.sm-draft-tag { display: inline-flex; align-items: center; height: 18px; padding: 0 6px; border-radius: 4px; font-size: 10.5px; font-weight: 700; background: var(--dsw-alias-state-warn-primary); color: var(--dsw-alias-label-primary-inverted); }
.sm-draft-head .sm-row-time { margin-left: auto; }
.sm-draft-body { white-space: pre-wrap; overflow-wrap: anywhere; padding: 7px 10px; border-radius: 8px; background: var(--dsw-alias-bg-base); border: 1px solid var(--dsw-alias-border-l2); font-size: 13px; }
.sm-draft-why { font-size: 11.5px; color: var(--dsw-alias-label-secondary); }
.sm-draft-actions { display: flex; gap: 6px; flex-wrap: wrap; align-items: center; }
.sm-draft-edit { display: grid; gap: 6px; }
.sm-draft .sm-textarea-box { min-height: 56px; background: var(--dsw-alias-bg-base); }

/* ——— "My alter": the owner ↔ alter transcript ——— */
.sm-citem { display: flex; flex-direction: column; max-width: 78%; gap: 2px; }
.sm-citem.sm-owner { align-self: flex-end; align-items: flex-end; }
.sm-citem.sm-alter { align-self: flex-start; align-items: flex-start; }
.sm-citem.sm-wide { max-width: 92%; align-self: flex-start; align-items: stretch; }
.sm-citem.sm-center { align-self: center; align-items: center; max-width: 90%; }
.sm-cmeta { display: flex; align-items: center; gap: 6px; font-size: 11px; color: var(--dsw-alias-label-tertiary); padding: 0 4px; }
.sm-cmeta .sm-proactive { font-size: 10px; font-weight: 700; color: var(--dsw-alias-brand-primary); background: var(--dsw-alias-interactive-bg-hover); padding: 0 6px; border-radius: 999px; }
.sm-obubble { padding: 7px 12px; border-radius: 14px 4px 14px 14px; background: var(--dsw-alias-brand-primary); color: var(--dsw-alias-label-primary-inverted); white-space: pre-wrap; overflow-wrap: anywhere; font-size: 13.5px; line-height: 20px; }
.sm-abubble { padding: 8px 12px; border-radius: 4px 14px 14px 14px; background: var(--dsw-specific-bubble, var(--dsw-alias-bg-layer-1)); border: 1px solid var(--dsw-alias-border-l2); color: var(--dsw-alias-label-primary); white-space: pre-wrap; overflow-wrap: anywhere; font-size: 13.5px; line-height: 20px; }
.sm-inmail { display: grid; gap: 3px; padding: 6px 10px; border-radius: 10px; border: 1px dashed var(--dsw-alias-border-l3, var(--dsw-alias-border-l2)); background: var(--dsw-alias-bg-layer-1, transparent); font-size: 12.5px; cursor: pointer; }
.sm-inmail:hover { background: var(--dsw-alias-interactive-bg-hover); }
.sm-inmail-head { display: flex; align-items: center; gap: 6px; font-size: 11.5px; color: var(--dsw-alias-label-secondary); }
.sm-inmail-head b { color: var(--dsw-alias-label-primary); }
.sm-inmail-body { white-space: pre-wrap; overflow-wrap: anywhere; color: var(--dsw-alias-label-primary); }
.sm-sendline { display: grid; gap: 3px; padding: 6px 10px; border-radius: 10px; border: 1px solid var(--dsw-alias-border-l2); background: var(--dsw-alias-bg-base); font-size: 12.5px; }
.sm-sendline-head { display: flex; align-items: center; gap: 6px; font-size: 11.5px; color: var(--dsw-alias-label-secondary); }
.sm-sendline-head b { color: var(--dsw-alias-label-primary); }
.sm-sendline-body { white-space: pre-wrap; overflow-wrap: anywhere; }
.sm-statepill { display: inline-flex; align-items: center; gap: 6px; padding: 2px 10px; border-radius: 999px; font-size: 11px; background: var(--dsw-alias-interactive-bg-hover); color: var(--dsw-alias-label-secondary); }
.sm-statepill.sm-ok { color: var(--dsw-alias-state-success-primary); }
.sm-statepill.sm-warn { color: var(--dsw-alias-state-warn-primary); background: var(--dsw-alias-state-warn-tertiary, var(--dsw-alias-interactive-bg-hover)); }
.sm-statepill.sm-err { color: var(--dsw-alias-state-error-primary); }
.sm-noteline { font-size: 11.5px; color: var(--dsw-alias-label-tertiary); text-align: center; padding: 2px 12px; }

/* ——— friend settings / protocol editor ——— */
.sm-alter-warn { color: var(--dsw-alias-state-warn-primary); }
.sm-alter-error { color: var(--dsw-alias-state-error-primary); }
.sm-alter-ok { color: var(--dsw-alias-state-success-primary); }
.sm-alter-actions { display: flex; gap: 6px; justify-content: flex-end; align-items: center; }
.sm-tier-pill { display: inline-flex; align-items: center; gap: 4px; padding: 0 6px; height: 18px; border-radius: 9px; font-size: 10.5px;
  background: var(--dsw-alias-interactive-bg-hover); color: var(--dsw-alias-label-secondary); border: 1px solid var(--dsw-alias-border-l2); }
.sm-tier-pill.sm-tier-auto { color: var(--dsw-alias-state-warn-primary); }
.sm-friend-settings { flex: none; margin: 0; padding: 10px 16px; display: grid; gap: 8px; border-top: 1px solid var(--dsw-alias-border-l2); background: var(--dsw-alias-bg-layer-1, var(--dsw-alias-bg-base)); font-size: 12px; max-height: 45%; overflow: auto; }
.sm-friend-settings label { display: grid; gap: 3px; }
.sm-friend-settings label > span { font-size: 11px; color: var(--dsw-alias-label-secondary); }
.sm-select { height: 26px; padding: 0 6px; font: inherit; font-size: 12px; border: 1px solid var(--dsw-alias-border-l2); border-radius: 8px; background: var(--dsw-alias-bg-base); color: inherit; }
.sm-textarea-box { width: 100%; min-height: 72px; max-height: 240px; padding: 6px 8px; box-sizing: border-box; resize: vertical; font: inherit; font-size: 12px; line-height: 18px;
  border: 1px solid var(--dsw-alias-border-l2); border-radius: 8px; background: var(--dsw-alias-bg-base); color: inherit; }
.sm-textarea-box:focus { outline: none; border-color: var(--dsw-alias-brand-primary); }
.sm-protocol { margin: 0 6px 8px; padding: 8px 10px; display: grid; gap: 6px; border-radius: 10px; border: 1px solid var(--dsw-alias-border-l2); background: var(--dsw-alias-bg-base); font-size: 12px; }
.sm-protocol .sm-textarea-box { min-height: 180px; font-family: var(--ds-font-family-code, ui-monospace, monospace); font-size: 11.5px; }
.sm-checkbox { display: inline-flex; align-items: center; gap: 6px; font-size: 11.5px; color: var(--dsw-alias-label-secondary); cursor: pointer; }

/* ——— group home (the group's profile page) ——— */
.sm-home { flex: 1; min-height: 0; overflow-y: auto; padding: 16px 20px; }
.sm-home-inner { max-width: 640px; margin: 0 auto; display: grid; gap: 12px; }
.sm-home-id { display: flex; align-items: center; gap: 12px; padding: 2px 2px 4px; }
.sm-home-id .sm-avatar { width: 44px; height: 44px; font-size: 18px; border-radius: 12px; }
.sm-home-id-name { font-size: 16px; font-weight: 600; }
.sm-home-id-sub { font-size: 12px; color: var(--dsw-alias-label-secondary); display: flex; gap: 6px; flex-wrap: wrap; align-items: center; }
.sm-home-card { border: 1px solid var(--dsw-alias-border-l2); border-radius: 12px; background: var(--dsw-alias-bg-layer-1, var(--dsw-alias-bg-base)); padding: 12px 14px; display: grid; gap: 8px; }
.sm-home-title { display: flex; align-items: center; gap: 8px; font-size: 12px; font-weight: 600; color: var(--dsw-alias-label-secondary); }
.sm-home-title > span:first-child { flex: 1; }
.sm-home-line { display: flex; gap: 8px; align-items: baseline; font-size: 13px; padding: 2px 0; }
.sm-home-line-key { flex: none; width: 110px; color: var(--dsw-alias-label-tertiary); font-size: 12px; }
.sm-home-line-val { flex: 1; min-width: 0; overflow-wrap: anywhere; color: var(--dsw-alias-label-primary); font-family: var(--ds-font-family-code, ui-monospace, monospace); font-size: 12px; }
.sm-member { display: flex; align-items: center; gap: 10px; padding: 6px 8px; border-radius: 8px; font-size: 13px; }
.sm-member:hover { background: var(--dsw-alias-interactive-bg-hover); }
.sm-rolepill { flex: none; padding: 1px 8px; border-radius: 999px; font-size: 10.5px; font-weight: 600; }
.sm-rolepill.sm-role-owner { background: var(--dsw-alias-brand-primary); color: var(--dsw-alias-label-primary-inverted); }
.sm-rolepill.sm-role-admin { border: 1px solid var(--dsw-alias-brand-primary); color: var(--dsw-alias-brand-primary); }
.sm-rolepill.sm-role-me { background: var(--dsw-alias-interactive-bg-hover); color: var(--dsw-alias-label-secondary); }
.sm-home-pin { display: flex; align-items: flex-start; gap: 8px; padding: 8px 10px; border-radius: 10px; background: var(--dsw-alias-bg-base); border: 1px solid var(--dsw-alias-border-l2); font-size: 12.5px; }
.sm-home-rules { white-space: pre-wrap; overflow-wrap: anywhere; font-size: 12.5px; line-height: 19px; color: var(--dsw-alias-label-primary); }

/* ——— composer: muted bar + the alter participation switch ——— */
.sm-mutebar { flex: none; display: flex; align-items: center; gap: 12px; margin: 8px 16px 12px; padding: 9px 14px; border-radius: 12px; border: 1px dashed var(--dsw-alias-border-l3, var(--dsw-alias-border-l2)); background: var(--dsw-alias-bg-layer-1, transparent); color: var(--dsw-alias-label-secondary); font-size: 12.5px; }
.sm-mutebar > span:first-child { flex: 1; }
.sm-switch { position: relative; display: inline-flex; align-items: center; gap: 7px; font-size: 12px; color: var(--dsw-alias-label-secondary); cursor: pointer; user-select: none; white-space: nowrap; }
.sm-switch input { position: absolute; opacity: 0; pointer-events: none; }
.sm-switch-track { width: 30px; height: 17px; border-radius: 999px; background: var(--dsw-alias-border-l3, var(--dsw-alias-border-l2)); position: relative; transition: background 120ms; flex: none; }
.sm-switch-track::after { content: ""; position: absolute; top: 2px; left: 2px; width: 13px; height: 13px; border-radius: 50%; background: var(--dsw-alias-bg-base); transition: transform 120ms; box-shadow: 0 1px 2px rgba(0, 0, 0, .25); }
.sm-switch input:checked + .sm-switch-track { background: var(--dsw-alias-brand-primary); }
.sm-switch input:checked + .sm-switch-track::after { transform: translateX(13px); }
.sm-switch input:disabled + .sm-switch-track { opacity: .5; }
.sm-switch.sm-on { color: var(--dsw-alias-label-primary); }

/* ——— the create-group dialog ——— */
.sm-modal-backdrop { position: absolute; inset: 0; z-index: 30; background: rgba(0, 0, 0, .28); display: flex; align-items: center; justify-content: center; animation: sm-page-in 100ms ease-out; }
.sm-modal { width: min(460px, calc(100% - 48px)); max-height: min(660px, calc(100% - 48px)); overflow-y: auto; box-sizing: border-box; border: 1px solid var(--dsw-alias-border-l1); border-radius: 14px; background: var(--dsw-alias-bg-layer-2, var(--dsw-alias-bg-base)); box-shadow: var(--dsw-shadow-lv3); padding: 16px 18px; display: grid; gap: 12px; font-size: 12.5px; }
.sm-modal-head { display: flex; align-items: center; gap: 8px; }
.sm-modal-title { flex: 1; font-size: 15px; font-weight: 600; }
.sm-field { display: grid; gap: 4px; }
.sm-field > span:first-child { font-size: 11px; font-weight: 600; color: var(--dsw-alias-label-tertiary); letter-spacing: .03em; text-transform: uppercase; }
.sm-input.sm-input-lg { height: 34px; font-size: 13px; padding: 0 10px; }
.sm-tplgrid { display: grid; grid-template-columns: 1fr 1fr; gap: 6px; }
.sm-tplcard { display: grid; gap: 2px; align-content: start; padding: 8px 10px; border: 1px solid var(--dsw-alias-border-l2); border-radius: 10px; background: var(--dsw-alias-bg-base); cursor: pointer; text-align: left; font: inherit; color: inherit; }
.sm-tplcard:hover { background: var(--dsw-alias-interactive-bg-hover); }
.sm-tplcard.sm-selected { border-color: var(--dsw-alias-brand-primary); box-shadow: inset 0 0 0 1px var(--dsw-alias-brand-primary); }
.sm-tplcard b { font-size: 12.5px; }
.sm-tplcard span { font-size: 11px; color: var(--dsw-alias-label-secondary); line-height: 15px; }
.sm-memberpick { display: grid; gap: 2px; max-height: 168px; overflow-y: auto; border: 1px solid var(--dsw-alias-border-l2); border-radius: 10px; padding: 4px; }
.sm-memberpick label { display: flex; align-items: center; gap: 8px; padding: 5px 8px; border-radius: 8px; cursor: pointer; font-size: 13px; }
.sm-memberpick label:hover { background: var(--dsw-alias-interactive-bg-hover); }
.sm-advgrid { display: grid; grid-template-columns: 1fr 1fr; gap: 8px 10px; }
.sm-advgrid .sm-field-full { grid-column: 1 / -1; }
.sm-modal-foot { display: flex; align-items: center; gap: 8px; justify-content: flex-end; }
.sm-modal-foot .sm-modal-note { flex: 1; font-size: 11.5px; color: var(--dsw-alias-label-secondary); }

/* ——— group chat: sender avatars + grouped runs ——— */
.sm-gline { display: flex; gap: 8px; align-items: flex-start; min-width: 0; }
.sm-gavatar { width: 24px; height: 24px; border-radius: 7px; font-size: 11px; font-weight: 600; display: flex; align-items: center; justify-content: center; flex: none; user-select: none; margin-top: 1px; }
.sm-gspacer { width: 24px; flex: none; }
.sm-gname { font-size: 11px; font-weight: 600; }

/* ——— two-step confirm ——— */
.sm-ghostbtn.sm-confirming, .sm-iconbtn.sm-confirming { border-color: var(--dsw-alias-state-error-primary); color: var(--dsw-alias-state-error-primary); background: var(--dsw-alias-interactive-bg-hover); }

/* ——— search results panel (names + message content, matches highlighted) ——— */
.sm-search-pop {
  position: absolute; top: 34px; left: 8px; right: 8px; z-index: 33; max-height: min(420px, 60vh); overflow-y: auto;
  border: 1px solid var(--dsw-alias-border-l1); border-radius: 12px; background: var(--dsw-alias-bg-layer-2, var(--dsw-alias-bg-base));
  box-shadow: var(--dsw-shadow-lv3); display: grid; padding: 4px; box-sizing: border-box;
}
.sm-search-hit {
  display: flex; align-items: center; gap: 10px; padding: 7px 8px; border: none; border-radius: 8px;
  background: none; font: inherit; color: inherit; text-align: left; cursor: pointer; min-width: 0;
}
.sm-search-hit:hover { background: var(--dsw-alias-interactive-bg-hover); }
.sm-search-hit-body { flex: 1; min-width: 0; display: grid; gap: 1px; }
.sm-hl { color: var(--dsw-alias-brand-primary); font-weight: 600; }

/* ——— the "+" menu (WeChat-style: one entry point for new group / add friend / join group) ——— */
.sm-plusmenu-wrap { position: relative; flex: none; }
.sm-plusmenu {
  position: absolute; top: 30px; right: 0; z-index: 32; min-width: 160px; padding: 4px; box-sizing: border-box;
  border: 1px solid var(--dsw-alias-border-l1); border-radius: 10px; background: var(--dsw-alias-bg-layer-2, var(--dsw-alias-bg-base));
  box-shadow: var(--dsw-shadow-lv3); display: grid; gap: 1px;
}
.sm-plusmenu button {
  display: flex; align-items: center; gap: 8px; padding: 7px 10px; border: none; border-radius: 7px;
  background: none; font: inherit; font-size: 13px; color: var(--dsw-alias-label-primary); cursor: pointer;
  text-align: left; white-space: nowrap;
}
.sm-plusmenu button:hover { background: var(--dsw-alias-interactive-bg-hover); }
.sm-plus-backdrop { position: fixed; inset: 0; z-index: 31; }

/* ——— Dark-mode brightening for the group speakers' inline hsl() colours
   (ChatRoom names / avatar glyphs). The Discord-flavoured dark palette itself
   is a theme token layer over the WHOLE shell (./Branding.tsx), so the page
   needs no token overrides of its own — it follows the host theme. ——— */
body[data-ds-dark-theme] .sm-page-root {
  --sm-hue-name-l: 72%;
  --sm-hue-avatar-l: 70%;
  --sm-hue-avatar-a: 0.16;
}
`

export function ensureStyles(): void {
  if (typeof document === 'undefined') return
  if (document.getElementById(STYLE_ID) !== null) return
  const style = document.createElement('style')
  style.id = STYLE_ID
  style.textContent = CSS
  document.head.appendChild(style)
}

export function removeStyles(): void {
  if (typeof document === 'undefined') return
  document.getElementById(STYLE_ID)?.remove()
}
