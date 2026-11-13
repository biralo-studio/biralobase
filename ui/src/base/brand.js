export function brand() {
    return t.div(
        { className: "brand" },
        t.img({ className: "main-logo", src: () => app.store.mainLogo, alt: "Biralobase black cat" }),
        t.span({ className: "brand-name" }, "Biralobase"),
    );
}
