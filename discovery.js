const { chromium } = require('playwright');
const fs = require('fs');

async function main() {
    console.log("Starting discovery session...");
    const browser = await chromium.launch({ headless: true });
    const page = await browser.newPage();

    async function printA11y() {
        const snapshot = await page.accessibility.snapshot();
        console.log(JSON.stringify(snapshot, null, 2));
    }

    // 1. Navigate to login
    await page.goto('https://www.saucedemo.com');
    await page.screenshot({ path: 'evidence/step1_login.png' });
    console.log("=== LOGIN PAGE A11Y ===");
    await printA11y();

    // 2. Login
    await page.fill('[data-test="username"]', 'standard_user');
    await page.fill('[data-test="password"]', 'secret_sauce');
    await page.click('[data-test="login-button"]');
    await page.waitForLoadState('networkidle');
    await page.screenshot({ path: 'evidence/step2_inventory.png' });
    console.log("=== INVENTORY PAGE A11Y ===");
    await printA11y();

    // 3. Add Backpack to cart
    await page.click('[data-test="add-to-cart-sauce-labs-backpack"]');
    await page.click('.shopping_cart_link');
    await page.waitForLoadState('networkidle');
    await page.screenshot({ path: 'evidence/step3_cart.png' });
    console.log("=== CART PAGE A11Y ===");
    await printA11y();

    // 4. Checkout Step 1
    await page.click('[data-test="checkout"]');
    await page.fill('[data-test="firstName"]', 'John');
    await page.fill('[data-test="lastName"]', 'Doe');
    await page.fill('[data-test="postalCode"]', '12345');
    await page.click('[data-test="continue"]');
    await page.waitForLoadState('networkidle');
    await page.screenshot({ path: 'evidence/step4_checkout_review.png' });
    console.log("=== CHECKOUT REVIEW A11Y ===");
    await printA11y();

    // 5. Finish (High Risk Step)
    await page.click('[data-test="finish"]');
    await page.waitForLoadState('networkidle');
    await page.screenshot({ path: 'evidence/step5_complete.png' });
    console.log("=== COMPLETE PAGE A11Y ===");
    await printA11y();

    // Error Path: Bad Login
    await page.goto('https://www.saucedemo.com');
    await page.fill('[data-test="username"]', 'standard_user');
    await page.fill('[data-test="password"]', 'wrong_password');
    await page.click('[data-test="login-button"]');
    await page.screenshot({ path: 'evidence/error_login.png' });
    console.log("=== ERROR PAGE A11Y ===");
    await printA11y();

    await browser.close();
}

main().catch(console.error);
