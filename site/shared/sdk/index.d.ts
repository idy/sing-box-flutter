export type Language = "en" | "zh";
export type LanguagePreference = Language | "auto";
export type Theme = "light" | "dark";
export type ThemePreference = Theme | "system";
/** Dictionary HTML is trusted author content only, when used with data-i18n-html. */
export type Translations = Partial<Record<Language, Record<string, string>>>;
export type EnhancementRoot = Document | Element | DocumentFragment;

export interface ReleaseAsset {
  name: string;
  browser_download_url: string;
  state: string;
  size: number;
  [key: string]: unknown;
}

export interface Release {
  tag_name: string;
  html_url: string;
  assets: ReleaseAsset[];
  draft?: boolean;
  prerelease?: boolean;
  published_at?: string | null;
  [key: string]: unknown;
}

export interface IdySite {
  readonly language: Language;
  readonly theme: Theme;
  t(key: string): string;
  /** Merge page translations and enhance the document. Repeated calls reuse the singleton. */
  init(translations?: Translations): IdySite;
  setTheme(preference: ThemePreference): void;
  setLanguage(preference: LanguagePreference): void;
  /** Translate and enhance newly inserted content; safe to call repeatedly. */
  enhance(root?: EnhancementRoot): IdySite;
  loadRelease(repository: string): Promise<Release>;
  asset(release: Release, pattern: RegExp): ReleaseAsset | undefined;
}

export interface SiteLink {
  label: string;
  /** HTTP(S), mailto, tel, or a relative URL. Unsafe URLs throw a TypeError. */
  href: string;
  /** Optional translation key for the label. */
  i18n?: string;
  ariaLabel?: string;
  ariaLabelKey?: string;
  newTab?: boolean;
  /** Phosphor icon name, used by header actions. */
  icon?: string;
}

export interface SiteBrandOptions {
  /** Defaults to the current browser document. Factories are inert until inserted and enhanced. */
  document?: Document;
  siteName?: string;
  siteHref?: string;
  projectName?: string;
  projectHref?: string;
}

export interface HeaderOptions extends SiteBrandOptions {
  /** Defaults to All projects, Discord, and GitHub; [] omits navigation. */
  navLinks?: readonly SiteLink[];
  /** Defaults to the GitHub follow link; null omits the action. */
  action?: SiteLink | null;
  /** Native language and theme selects are included by default. */
  preferences?: boolean;
}

export interface FooterOptions extends SiteBrandOptions {
  note?: string;
  noteKey?: string;
  /** Defaults to All projects, Discord, and GitHub; [] omits navigation. */
  links?: readonly SiteLink[];
}

/** Explicit browser initialization. Importing this module is safe during SSR. */
export function initSite(translations?: Translations): IdySite;
export function enhance(root?: EnhancementRoot): IdySite;
export function setTheme(preference: ThemePreference): void;
export function setLanguage(preference: LanguagePreference): void;
export function loadRelease(repository: string): Promise<Release>;
export function asset(release: Release, pattern: RegExp): ReleaseAsset | undefined;
export function createHeader(options?: HeaderOptions): HTMLElement;
export function createFooter(options?: FooterOptions): HTMLElement;

declare global {
  interface Window {
    IdySite?: IdySite;
    IdySiteSDK?: {
      initSite: typeof initSite;
      enhance: typeof enhance;
      setTheme: typeof setTheme;
      setLanguage: typeof setLanguage;
      loadRelease: typeof loadRelease;
      asset: typeof asset;
      createHeader: typeof createHeader;
      createFooter: typeof createFooter;
    };
  }
}
