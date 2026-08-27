/**
 * SoulMirror branding + the Discord-flavoured dark palette, applied to the
 * WHOLE dsh shell (host sidebar included) so the app reads as one product.
 *
 * Everything goes through sanctioned extension points — no node_modules
 * edits, no DOM surgery on host-owned elements:
 *   - `sidebar.brand.mark` / `sidebar.brand.name` are single slots declared
 *     by ui-sidebar ("deployments may replace the shell's fallback"); the
 *     stock deepseek occupant sits at priority 0 and single slots shadow by
 *     priority (lowest renders), so our occupants register at -10.
 *   - the `theme` service's `overrideTokens` stacks a token layer over the
 *     built-in themes; ui-layout's ThemePresenter writes the composed tokens
 *     as inline body properties, so the layer recolors host and page alike.
 *     The layer maps each token to { light, dark }: light repeats the stock
 *     light values (light mode stays stock dsh), dark carries the Discord
 *     palette the owner picked (2026-08-24).
 *   - `setTheme('dark')` runs only while the durable preference is still the
 *     schema default (`system`) — SoulMirror ships dark-first, but a user
 *     choice made in Settings → Appearance is never overridden.
 *   - the browser title and favicon have no slot; a ctx.effect swaps them and
 *     an observer pins the title against host rewrites (SPA title updates).
 */
import type { ClientContext } from '@deepseek-ai/dsh-client-runtime/client'
import type {} from '@deepseek-ai/dsh-client-ui-theme/client'
import { BRAND_FAVICON, BRAND_MARK, BRAND_NAME } from './brand-assets.ts'

/** Shadow priority for the brand slots (single slots: lowest priority renders). */
const BRAND_PRIORITY = -10
/** Layer source name shown by theme inspection for our token overrides. */
const THEME_SOURCE = 'soulnet-dsh'

/** Sidebar brand mark occupant: the SoulMirror icon at the requested edge. */
function BrandMark({ size }: { size: number }) {
  return <img src={BRAND_MARK} width={size} height={size} alt="" style={{ display: 'block', borderRadius: 6 }} />
}

/** Sidebar brand name occupant: the product wordmark as themed text. */
function BrandName() {
  return <span style={{ fontWeight: 600, fontSize: 15, letterSpacing: '.01em' }}>{BRAND_NAME}</span>
}

/**
 * The Discord-flavoured dark palette as a theme token layer. Light values are
 * the stock dsh light palette (visually a no-op there); dark values follow the
 * reference: layered greys #1e1f22/#2b2d31/#313338, blurple #5865f2 brand,
 * #dbdee1/#b5bac1/#949ba4 text.
 */
const DISCORD_TOKENS: Record<string, { light: string; dark: string }> = {
  '--dsw-alias-bg-base': { light: '#ffffff', dark: '#313338' },
  '--dsw-alias-bg-layer-1': { light: '#ffffff', dark: '#2b2d31' },
  '--dsw-alias-bg-layer-2': { light: '#ffffff', dark: '#383a40' },
  '--dsw-alias-bg-layer-3': { light: '#ffffff', dark: '#383a40' },
  '--dsw-specific-sidebar-fill': { light: '#f9fafb', dark: '#2b2d31' },
  '--dsw-specific-bubble': { light: '#edf3fe', dark: '#2b2d31' },
  '--dsw-specific-bubble-highlight': { light: '#d3e2ff', dark: '#404249' },
  '--dsw-specific-input-major': { light: '#ffffff', dark: '#383a40' },
  '--dsw-specific-selector': { light: '#f9fafb', dark: '#383a40' },
  '--dsw-specific-tip': { light: '#f9fafb', dark: '#383a40' },
  '--dsw-alias-label-primary': { light: '#0f1115', dark: '#dbdee1' },
  '--dsw-alias-label-secondary': { light: '#61666b', dark: '#b5bac1' },
  '--dsw-alias-label-tertiary': { light: '#81858c', dark: '#949ba4' },
  '--dsw-alias-label-primary-inverted': { light: '#ffffff', dark: '#0f1115' },
  '--dsw-alias-brand-primary': { light: '#0f1115', dark: '#5865f2' },
  '--dsw-alias-button-primary-hover': { light: '#43454a', dark: '#4752c4' },
  '--dsw-alias-interactive-bg-hover': { light: '#2631480f', dark: '#4e505859' },
  '--dsw-alias-interactive-bg-hover-solid': { light: '#f1f3f5', dark: '#35373c' },
  '--dsw-specific-sidebar-nav-item-active': { light: '#ebeef2', dark: '#404249' },
  '--dsw-specific-sidebar-nav-item-hover': { light: '#f1f3f5', dark: '#35373c' },
  '--dsw-specific-sidebar-nav-item-active-accent': { light: '#e4edfd', dark: '#3c3f45' },
  '--dsw-alias-markdown-code-block': { light: '#f9fafb', dark: '#2b2d31' },
}

/** Install branding (brand slots, title, favicon) and the palette layer. */
export function installBranding(ctx: ClientContext): void {
  // Palette layer + dark-first default (a made preference is never overridden).
  ctx.effect(() => ctx.theme.overrideTokens(THEME_SOURCE, DISCORD_TOKENS), 'soulmirror: discord palette layer')
  if (ctx.theme.getTheme().preference === 'system') ctx.theme.setTheme('dark')

  // Brand slots (register once the sidebar declares them; stock dsh always does).
  ctx.slots.inject('sidebar.brand.mark', () => ctx.slots.register({ name: 'sidebar.brand.mark', priority: BRAND_PRIORITY }, BrandMark))
  ctx.slots.inject('sidebar.brand.name', () => ctx.slots.register({ name: 'sidebar.brand.name', priority: BRAND_PRIORITY }, BrandName))

  // Browser title + favicon (no slot exists for either).
  ctx.effect(() => {
    if (typeof document === 'undefined') return () => {}
    const prevTitle = document.title
    document.title = BRAND_NAME
    const titleEl = document.querySelector('title')
    const observer = new MutationObserver(() => {
      if (document.title !== BRAND_NAME) document.title = BRAND_NAME
    })
    if (titleEl !== null) observer.observe(titleEl, { childList: true, characterData: true, subtree: true })
    const icon = document.querySelector<HTMLLinkElement>('link[rel~="icon"]')
    const prev = icon !== null ? { href: icon.href, type: icon.type } : undefined
    if (icon !== null) {
      icon.href = BRAND_FAVICON
      icon.type = 'image/png'
    }
    return () => {
      observer.disconnect()
      document.title = prevTitle
      if (icon !== null && prev !== undefined) {
        icon.href = prev.href
        icon.type = prev.type
      }
    }
  }, 'soulmirror: title + favicon')
}
