type AsciiHeroProps = {
  ascii: string;
  subtitle: string;
};

export function AsciiHero({ ascii, subtitle }: AsciiHeroProps) {
  const content = ascii.trimEnd().length > 0 ? ascii.trimEnd() : "AREYOUAI";
  const maxColumns = content.split("\n").reduce((lineMax, line) => Math.max(lineMax, line.length), 0);

  return (
    <section className="ascii-hero">
      <div className="ascii-wrap">
        <pre className="ascii-frame show" style={{ width: `${maxColumns}ch` }}>
          {content}
        </pre>
      </div>
      <p className="ascii-subtitle">{subtitle}</p>
    </section>
  );
}
