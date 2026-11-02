// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

const site = 'https://tailreach.tanmaylabs.me';

export default defineConfig({
  site,
  integrations: [
    starlight({
      title: 'tailreach',
      description: 'Your Mac, in any browser. Over your own Tailscale.',
      logo: { src: './src/assets/logo.png' },
      favicon: '/favicon.png',
      social: [{ icon: 'github', label: 'GitHub', href: 'https://github.com/TanmayDoesAI/tailreach' }],
      editLink: { baseUrl: 'https://github.com/TanmayDoesAI/tailreach/edit/main/site/' },
      customCss: ['./src/styles/theme.css'],
      head: [
        { tag: 'link', attrs: { rel: 'apple-touch-icon', href: '/apple-touch-icon.png' } },
        { tag: 'meta', attrs: { property: 'og:image', content: `${site}/og.jpg` } },
        { tag: 'meta', attrs: { name: 'twitter:card', content: 'summary_large_image' } },
      ],
      sidebar: [
        {
          label: 'Docs',
          items: [
            { label: 'Getting started', slug: 'docs/getting-started' },
            { label: 'Using it', slug: 'docs/using-it' },
            { label: 'Guests', slug: 'docs/guests' },
            { label: 'Security', slug: 'docs/security' },
            { label: 'Troubleshooting', slug: 'docs/troubleshooting' },
            { label: 'How the stream works', slug: 'docs/streaming' },
            { label: 'FAQ', slug: 'docs/faq' },
          ],
        },
      ],
    }),
  ],
});
