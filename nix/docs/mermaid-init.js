// nix/docs/mermaid-init.js
//
// Replaces the mermaid-init.js that `mdbook-mermaid install` writes beside
// mermaid.min.js. Upstream's copy was built against mdBook 0.5.0, where the
// theme picker's buttons were `id="navy"`, `id="ayu"` and so on; mdBook 0.5.2
// renamed them to `id="mdbook-theme-navy"` and added an "Auto" entry that
// follows prefers-color-scheme. So upstream's script throws on the first
// getElementById and every diagram stays in the colours it was drawn with,
// however the reader switches theme afterwards.
//
// Rather than pin the new ids — which is the same bet again — this watches the
// `class` attribute on <html>, which is where mdBook records the resolved
// theme no matter which control changed it. And it re-renders in place instead
// of reloading the page: mermaid consumes the <pre> it draws into, so the
// diagram source is stashed on first pass and put back before each redraw.

(() => {
    const darkThemes = new Set(['ayu', 'coal', 'navy']);
    const html = document.documentElement;

    // Captured before mermaid runs: this script is parser-blocking and sits
    // ahead of DOMContentLoaded, so the fences are still text at this point.
    const blocks = Array.from(document.querySelectorAll('pre.mermaid'));
    const sources = blocks.map((block) => block.textContent);

    if (blocks.length === 0) {
        return;
    }

    const themeNow = () =>
        Array.from(html.classList).some((cls) => darkThemes.has(cls)) ? 'dark' : 'default';

    let drawn = null;

    const draw = () => {
        const theme = themeNow();
        if (theme === drawn) {
            return;
        }
        drawn = theme;

        blocks.forEach((block, i) => {
            block.textContent = sources[i];
            block.removeAttribute('data-processed');
        });

        // startOnLoad: false — the redraw below is the only thing that renders,
        // so mermaid's own DOMContentLoaded pass would be a duplicate.
        mermaid.initialize({ startOnLoad: false, theme });
        mermaid.run({ nodes: blocks });
    };

    draw();
    new MutationObserver(draw).observe(html, {
        attributes: true,
        attributeFilter: ['class'],
    });
})();
