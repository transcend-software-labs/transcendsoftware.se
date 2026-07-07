import satori from "satori";
import { Resvg } from "@resvg/resvg-js";
import { readFileSync } from "fs";
import { resolve } from "path";

const fontRegular = readFileSync(
  resolve("src/fonts/JetBrainsMono-Regular.ttf")
);
const fontBold = readFileSync(
  resolve("src/fonts/JetBrainsMono-Bold.ttf")
);

export async function GET() {
  const svg = await satori(
    {
      type: "div",
      props: {
        // Outer background (the gray "stack" peek behind the card)
        style: {
          width: "1200px",
          height: "630px",
          background: "#e8e6e0",
          display: "flex",
          alignItems: "center",
          justifyContent: "center",
          fontFamily: "JetBrains Mono",
        },
        children: [
          // Shadow card (offset, visible behind the main card)
          {
            type: "div",
            props: {
              style: {
                position: "absolute",
                top: "52px",
                left: "64px",
                width: "1068px",
                height: "506px",
                background: "#c9c7c0",
                border: "2px solid #b0aea8",
                display: "flex",
              },
              children: " ",
            },
          },
          // Main card
          {
            type: "div",
            props: {
              style: {
                position: "absolute",
                top: "40px",
                left: "48px",
                width: "1068px",
                height: "506px",
                background: "#faf9f6",
                border: "2px solid #1a1917",
                display: "flex",
                flexDirection: "column",
                justifyContent: "space-between",
                padding: "56px 64px 48px 64px",
              },
              children: [
                // Top: name + title
                {
                  type: "div",
                  props: {
                    style: { display: "flex", flexDirection: "column" },
                    children: [
                      {
                        type: "div",
                        props: {
                          style: {
                            display: "flex",
                            fontSize: "82px",
                            fontWeight: 700,
                            color: "#1a1917",
                            lineHeight: "1.1",
                            letterSpacing: "-0.03em",
                          },
                          children: "Rasmus Kockum",
                        },
                      },
                      {
                        type: "div",
                        props: {
                          style: {
                            display: "flex",
                            fontSize: "34px",
                            fontWeight: 400,
                            color: "#6b6965",
                            marginTop: "18px",
                          },
                          children: "Senior Engineer & Tech Lead",
                        },
                      },
                    ],
                  },
                },
                // Bottom row
                {
                  type: "div",
                  props: {
                    style: {
                      display: "flex",
                      justifyContent: "space-between",
                      alignItems: "flex-end",
                    },
                    children: [
                      {
                        type: "div",
                        props: {
                          style: {
                            display: "flex",
                            fontSize: "22px",
                            fontWeight: 400,
                            color: "#6b6965",
                            maxWidth: "640px",
                          },
                          children: "Java backends at 20k+ RPS · React · cloud infrastructure",
                        },
                      },
                      {
                        type: "div",
                        props: {
                          style: {
                            fontSize: "22px",
                            fontWeight: 700,
                            color: "#1a1917",
                          },
                          children: "Transcend Software",
                        },
                      },
                    ],
                  },
                },
              ],
            },
          },
        ],
      },
    },
    {
      width: 1200,
      height: 630,
      fonts: [
        {
          name: "JetBrains Mono",
          data: fontRegular,
          weight: 400,
          style: "normal",
        },
        {
          name: "JetBrains Mono",
          data: fontBold,
          weight: 700,
          style: "normal",
        },
      ],
    }
  );

  const resvg = new Resvg(svg, {
    fitTo: { mode: "width", value: 1200 },
  });
  const png = resvg.render().asPng();

  return new Response(png, {
    headers: {
      "Content-Type": "image/png",
      "Cache-Control": "public, max-age=31536000, immutable",
    },
  });
}
