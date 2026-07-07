import rss from "@astrojs/rss";
import { getCollection } from "astro:content";
import { sortPostsByDate } from "../lib/posts";

export async function GET() {
  const posts = sortPostsByDate(await getCollection("posts"));
  return rss({
    title: "Transcend Software Blog",
    description: "Blog about software engineering by Rasmus at Transcend Software.",
    site: "https://transcendsoftware.se",
    items: posts.map((post) => ({
      title: post.data.title,
      description: post.data.description,
      pubDate: post.data.date,
      link: `/posts/${post.slug}`,
    })),
    customData: `<language>en</language>`,
  });
}
