// @ts-check
import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';
import sitemap from '@astrojs/sitemap';
import starlightLlmsTxt from 'starlight-llms-txt';

export default defineConfig({
  // Canonical site URL used for the sitemap and llms.txt.
  // After the first deploy, Workers prints the exact *.workers.dev URL —
  // update this line (or point it at the final custom domain) and redeploy.
  site: 'https://graymatter-docs.workers.dev',
  integrations: [
    starlight({
      title: 'GrayMatter',
      description:
        'Persistent memory, a self-building knowledge graph, and 90% fewer context tokens for AI agents. One binary, zero infra.',
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: 'https://github.com/angelnicolasc/graymatter',
        },
      ],
      customCss: ['./src/styles/custom.css'],
      plugins: [starlightLlmsTxt()],
      sidebar: [
        {
          label: 'Start here',
          items: [
            { label: 'Quick start', link: '/guides/quickstart' },
            { label: 'MCP clients', link: '/guides/mcp-clients' },
          ],
        },
        {
          label: 'Guides',
          items: [
            { label: 'Knowledge graph', link: '/guides/knowledge-graph' },
            { label: 'Observability', link: '/guides/observability' },
            { label: 'Go library', link: '/guides/go-library' },
          ],
        },
        {
          label: 'Reference',
          items: [
            { label: 'Agent guide', link: '/reference/agents-guide' },
            { label: 'Benchmarks', link: '/reference/benchmarks' },
            { label: 'API stability', link: '/reference/api-stability' },
            { label: 'Plugin protocol', link: '/reference/plugin-protocol' },
            { label: 'Threat model', link: '/reference/threat-model' },
          ],
        },
        {
          label: 'Design decisions',
          items: [{ autogenerate: { directory: 'reference/decisions' } }],
        },
      ],
    }),
    sitemap(),
  ],
});
