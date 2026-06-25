/** Primary nav links (plan §6.0). Docs/GitHub point at the repo for now. */
export const GITHUB_URL = "https://github.com/wierdbytes/autosk";
export const DOCS_URL = "https://github.com/wierdbytes/autosk/tree/main/docs";
export const RELEASES_URL = "https://github.com/wierdbytes/autosk/releases";
export const CHANGELOG_URL = "https://github.com/wierdbytes/autosk/blob/main/CHANGELOG.md";
export const LICENSE_URL = "https://github.com/wierdbytes/autosk/blob/main/LICENSE";
export const LATEST_RELEASE_URL = "https://github.com/wierdbytes/autosk/releases/latest";
/** Public TestFlight invite for the iOS app (beta). */
export const TESTFLIGHT_URL = "https://testflight.apple.com/join/jYDpqT2v";

export type NavLink = { label: string; href: string };

export const navLinks: NavLink[] = [
  { label: "Features", href: "#features" },
  { label: "How it works", href: "#how-it-works" },
  { label: "Docs", href: DOCS_URL },
  { label: "GitHub", href: GITHUB_URL },
];

/** The macOS install command surfaced in the hero + final CTA. */
export const BREW_INSTALL = "brew install --cask wierdbytes/autosk/autosk";
